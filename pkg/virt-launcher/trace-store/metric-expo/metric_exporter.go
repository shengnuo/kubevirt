package metricexpo

import "time"

type MetricExporter struct {
	Name      string        `json:"name"`
	Namespace string        `json:"namespace"`
	StageName string        `json:"stagename"`
	Duration  time.Duration `json:"duration"`
}

func (me *MetricExporter) GetIdentifier() string {
	return me.Namespace + "/" + me.Name
}
