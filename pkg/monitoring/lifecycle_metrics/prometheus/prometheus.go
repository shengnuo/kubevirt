package prometheus

import (
	"time"

	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/trace-store/metric-expo"

	"github.com/prometheus/client_golang/prometheus"
	"kubevirt.io/client-go/log"
	// aggregator "kubevirt.io/kubevirt/pkg/monitoring/lifecycle_metrics/aggregator"
)

var (
	durationSummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Subsystem: "lifecycle_metrics",
			Name:      "kubevirt_lifecycle_duration_summary",
			Help:      "Duration summary of kubevirt lifecycle stages",
		},
		[]string{"stage"},
	)

	durationGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: "lifecycle_metrics",
			Name:      "kubevirt_lifecycle_duration_gauge",
			Help:      "Duration of kubevirt lifecycle stages",
		},
		[]string{"namespace", "name", "stage"},
	)
)

// type prometheusScraper struct {
// 	ch chan<- prometheus.Metric
// }

// func (ps *prometheusScraper) Scrape(
// 	summaryAggregators map[string]*aggregator.SummaryAggregator,
// 	newRecord map[string]map[string]map[string]time.Duration) {
// 	for stage, sumAgg := range summaryAggregators {
// 		mv, err := prometheus.NewConstSummary(
// 			durationSummaryDesc,
// 			sumAgg.GetCount(),
// 			sumAgg.GetSum(),
// 			map[float64]float64{0.5: 0.23, 0.99: 0.56},
// 			stage,
// 		)
// 		if err == nil {
// 			ps.ch <- mv
// 		} else {
// 			log.Log.Reason(err).Error("Failed to push duration summary metrics")
// 		}
// 	}

// 	for namespace, nsMap := range newRecord {
// 		for name, nameMap := range nsMap {
// 			for stage, duration := range nameMap {
// 				mv, err := prometheus.NewConstMetric(
// 					durationDesc,
// 					prometheus.GaugeValue,
// 					float64(duration),
// 					namespace,
// 					name,
// 					stage,
// 				)
// 				if err == nil {
// 					ps.ch <- mv
// 				} else {
// 					log.Log.Reason(err).Error("Failed to push duration metrics")
// 				}
// 			}
// 		}
// 	}

// }

func Update(exporter *metricexpo.MetricExporter) {
	log.Log.Info("pushing prometheus metrics")
	durationSecond := float64(exporter.Duration) / float64(time.Second)

	durationSummary.With(
		prometheus.Labels{
			"stage": exporter.StageName,
		},
	).Observe(durationSecond)

	durationGauge.With(
		prometheus.Labels{
			"namespace": exporter.Namespace,
			"name":      exporter.Name,
			"stage":     exporter.StageName,
		},
	).Add(durationSecond)
}

func init() {
	log.Log.Info("registering lifecycle collector")
	prometheus.MustRegister(durationGauge)
	prometheus.MustRegister(durationSummary)
}
