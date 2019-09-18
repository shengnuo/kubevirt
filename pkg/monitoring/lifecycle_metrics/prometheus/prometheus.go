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
		[]string{"namespace", "name", "stage", "uid"},
	)
)

func Update(exporter *metricexpo.MetricExporter) {
	log.Log.Info("pushing prometheus metrics")
	durationSecond := float64(exporter.Duration) / float64(time.Second)

	durationSummary.With(
		prometheus.Labels{
			"stage": exporter.StageName,
		},
	).Observe(durationSecond)

	// durationGauge._metrics.clear()
	durationGauge.With(
		prometheus.Labels{
			"namespace": exporter.Namespace,
			"name":      exporter.Name,
			"stage":     exporter.StageName,
			"uid":       exporter.UID,
		},
	).Add(durationSecond)
}

func init() {
	log.Log.Info("registering lifecycle collector")
	prometheus.MustRegister(durationGauge)
	prometheus.MustRegister(durationSummary)
}
