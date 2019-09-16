package metricexpo

import "time"

type MetricExporter struct {
	Name      string        `json:"name"`
	Namespace string        `json:"namespace"`
	StageName string        `json:"stagename"`
	UID       string        `json:"uid"`
	Duration  time.Duration `json:"duration"`
}

func (me *MetricExporter) GetIdentifier() string {
	return me.Namespace + "/" + me.Name + "/" + me.UID
}
