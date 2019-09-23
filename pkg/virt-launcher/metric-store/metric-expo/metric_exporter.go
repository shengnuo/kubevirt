package metricexpo

import "time"

type MetricExporter struct {
	Name          string        `json:"name"`
	Namespace     string        `json:"namespace"`
	LifecycleName string        `json:"lifecyclename"`
	UID           string        `json:"uid"`
	Duration      time.Duration `json:"duration"`
}
