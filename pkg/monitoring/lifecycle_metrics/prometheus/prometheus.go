package prometheus

import (
	"sync"
	"time"

	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/metric-store/metric-expo"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	durationSummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Subsystem: "kubevirt",
			Name:      "lifecycle_duration_summary",
			Help:      "Duration summary of kubevirt lifecycle stages",
		},
		[]string{"stage"},
	)

	durationGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: "kubevirt",
			Name:      "lifecycle_duration_gauge",
			Help:      "Duration of kubevirt lifecycle stages",
		},
		[]string{"namespace", "name", "stage", "uid"},
	)
)

func Update(exporter *metricexpo.MetricExporter) {
	durationSecond := float64(exporter.Duration) / float64(time.Second)

	durationSummary.With(
		prometheus.Labels{
			"stage": exporter.LifecycleName,
		},
	).Observe(durationSecond)

	durationGauge.With(
		prometheus.Labels{
			"namespace": exporter.Namespace,
			"name":      exporter.Name,
			"stage":     exporter.LifecycleName,
			"uid":       exporter.UID,
		},
	).Set(durationSecond)
}

var once sync.Once

func init() {
	once.Do(func() {
		prometheus.MustRegister(durationGauge)
		prometheus.MustRegister(durationSummary)
	})
}
