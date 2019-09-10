package aggregator

import (
	"fmt"
	"sync"
	"time"

	prometheus "kubevirt.io/kubevirt/pkg/monitoring/lifecycle_metrics/prometheus"
	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/trace-store/metric-expo"
)

type SummaryAggregator struct {
	count uint64
	sum   float64
}

func (sa *SummaryAggregator) GetCount() uint64 {
	return sa.count
}

func (sa *SummaryAggregator) GetSum() float64 {
	return sa.sum
}

func (sa *SummaryAggregator) Observe(me *metricexpo.MetricExporter) {
	d := me.Duration
	sa.sum += float64(d) / float64(time.Second)
	sa.count += 1
}

type LifecycleMetricsAggregator struct {
	// map[string]hashset[string],
	//	key: vm name
	//	id: stagename
	lock               sync.Mutex
	updatedFields      map[string]map[string]bool
	summaryAggregators map[string]*SummaryAggregator

	// namespace:name:stage:duration
	newRecords map[string]map[string]map[string]time.Duration
}

func (a *LifecycleMetricsAggregator) UpdateAggregator(exporter *metricexpo.MetricExporter) {
	a.lock.Lock()
	defer a.lock.Unlock()

	vmID := exporter.GetIdentifier()

	// check if the vm exists in the aggregator
	_, exists := a.updatedFields[vmID]
	if !exists {
		a.updatedFields[vmID] = make(map[string]bool)
	}

	stageHashsetWithVmid := a.updatedFields[vmID]

	stage := exporter.StageName
	// check if the given stage is seen for the first time in general
	if _, exists = a.summaryAggregators[stage]; !exists {
		a.summaryAggregators[stage] = &SummaryAggregator{
			count: 0,
			sum:   0.0,
		}
	}

	// vmid:stage not yet recorded
	if _, exists = stageHashsetWithVmid[stage]; !exists {
		stageHashsetWithVmid[stage] = true
		a.summaryAggregators[stage].Observe(exporter)
		a.addRecord(exporter)
	}
	prometheus.Update(exporter)
}

func (a *LifecycleMetricsAggregator) addRecord(exporter *metricexpo.MetricExporter) {
	var exists bool

	if _, exists = a.newRecords[exporter.Namespace]; !exists {
		a.newRecords[exporter.Namespace] = make(map[string]map[string]time.Duration)
	}
	nsMap := a.newRecords[exporter.Namespace]

	if _, exists = nsMap[exporter.Name]; !exists {
		nsMap[exporter.Name] = make(map[string]time.Duration)
	}
	nameMap := nsMap[exporter.Name]

	if _, exists = nameMap[exporter.StageName]; !exists {
		nameMap[exporter.StageName] = exporter.Duration
	}
}

func (a *LifecycleMetricsAggregator) clearRecords() {
	a.newRecords = make(map[string]map[string]map[string]time.Duration)
}

func (a *LifecycleMetricsAggregator) Print() {
	fmt.Println("-------------------------------------------------------------------------------")
	fmt.Println(a.updatedFields)
	for k, v := range a.summaryAggregators {
		fmt.Printf("%s: count=%d,  sum=%f\n", k, v.GetCount(), v.GetSum())
	}
}

type metricsScraper interface {
	Scrape(
		summaryAggregators map[string]*SummaryAggregator,
		newRecord map[string]map[string]map[string]time.Duration,
	)
}

func (a *LifecycleMetricsAggregator) PrometheusUpdate(scraper metricsScraper) {
	a.lock.Lock()
	defer a.lock.Unlock()

	scraper.Scrape(a.summaryAggregators, a.newRecords)
	a.clearRecords()
}

var aggregator *LifecycleMetricsAggregator
var once sync.Once

func Collect(scraper metricsScraper) {
	agg := GetAggregator()
	agg.PrometheusUpdate(scraper)

}

func GetAggregator() *LifecycleMetricsAggregator {
	once.Do(func() {
		aggregator = &LifecycleMetricsAggregator{
			updatedFields:      make(map[string]map[string]bool),
			summaryAggregators: make(map[string]*SummaryAggregator),
			newRecords:         make(map[string]map[string]map[string]time.Duration),
		}
	})
	return aggregator
}
