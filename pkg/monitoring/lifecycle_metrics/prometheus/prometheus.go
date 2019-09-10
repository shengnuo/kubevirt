package prometheus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"kubevirt.io/client-go/log"
	aggregator "kubevirt.io/kubevirt/pkg/monitoring/lifecycle_metrics/aggregator"
)

var (
	durationSummaryDesc = prometheus.NewDesc(
		"kubevirt_lifecycle_duration_summary",
		"Duration summary of kubevirt lifecycles",
		[]string{"stage"},
		nil,
	)

	durationDesc = prometheus.NewDesc(
		"kubevirt_lifecycle_duration",
		"Duration of kubevirt lifecycles",
		[]string{"namespace", "name", "stage"},
		nil,
	)
)

type prometheusScraper struct {
	ch chan<- prometheus.Metric
}

func (ps *prometheusScraper) Scrape(
	summaryAggregators map[string]*aggregator.SummaryAggregator,
	newRecord map[string]map[string]map[string]time.Duration) {
	for stage, sumAgg := range summaryAggregators {
		mv, err := prometheus.NewConstSummary(
			durationSummaryDesc,
			sumAgg.GetCount(),
			sumAgg.GetSum(),
			map[float64]float64{0.5: 0.23, 0.99: 0.56},
			stage,
		)
		if err == nil {
			ps.ch <- mv
		} else {
			log.Log.Reason(err).Error("Failed to push duration summary metrics")
		}
	}

	for namespace, nsMap := range newRecord {
		for name, nameMap := range nsMap {
			for stage, duration := range nameMap {
				mv, err := prometheus.NewConstMetric(
					durationDesc,
					prometheus.GaugeValue,
					float64(duration),
					namespace,
					name,
					stage,
				)
				if err == nil {
					ps.ch <- mv
				} else {
					log.Log.Reason(err).Error("Failed to push duration metrics")
				}
			}
		}
	}

}

type Collector struct {
}

func init() {
	log.Log.Info("registering lifecycle collector")
	prometheus.MustRegister(&Collector{})
}

func (co *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- durationSummaryDesc
	ch <- durationDesc
}

// this function will be called concurrently
func (co *Collector) Collect(ch chan<- prometheus.Metric) {
	scraper := &prometheusScraper{ch: ch}
	aggregator.Collect(scraper)
}
