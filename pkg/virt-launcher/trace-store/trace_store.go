package tracestore

import (
	"errors"
	"fmt"
	"time"

	"kubevirt.io/client-go/log"
	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/trace-store/metric-expo"

	notifyclient "kubevirt.io/kubevirt/pkg/virt-launcher/notify-client"
)

type StageDuration struct {
	StartTime  time.Time `json:"startTime"`
	FinishTime time.Time `json:"finishTime"`
}

func StartStage() *StageDuration {
	return &StageDuration{
		StartTime: time.Now(),
	}
}

func (sd *StageDuration) FinishStage() error {
	if sd.FinishTime.IsZero() {
		sd.FinishTime = time.Now()
		fmt.Println(sd.FinishTime)
	} else {
		fmt.Println("finish time already exists")
	}
	return nil
}

func (sd *StageDuration) CalculateDuration() (time.Duration, error) {
	if sd.StartTime.IsZero() || sd.FinishTime.IsZero() {
		return 0, errors.New("start time or finish time is zero")
	}

	return sd.FinishTime.Sub(sd.StartTime), nil
}

type TraceStore struct {
	Name           string                    `json:"name"`
	UID            string                    `json:"uid"`
	Namespace      string                    `json:"namespace"`
	StageDurations map[string]*StageDuration `json:"stageDurations"`
	notifier       *notifyclient.Notifier
}

func NewTraceStore(name, uid, namespace string) *TraceStore {
	return &TraceStore{
		Name:           name,
		UID:            uid,
		Namespace:      namespace,
		StageDurations: make(map[string]*StageDuration),
	}
}

func (ts *TraceStore) NewStage(stageName string) error {
	if _, exists := ts.StageDurations[stageName]; exists {
		return errors.New("stage already exists!")
	}
	ts.StageDurations[stageName] = StartStage()
	return nil
}

func (ts *TraceStore) reportStage(stageName string) {
	log.Log.Info("reporting metrics")
	d, _ := ts.Duration(stageName)
	ts.notifier.SendLifecycleMetrics(metricexpo.MetricExporter{
		Namespace: ts.Namespace,
		Name:      ts.Name,
		StageName: stageName,
		Duration:  d,
	})
}

func (ts *TraceStore) UpdateNotifier(notifier *notifyclient.Notifier) {
	ts.notifier = notifier
}

func (ts *TraceStore) FinishStage(stageName string) error {
	if v, exists := ts.StageDurations[stageName]; exists {

		e := v.FinishStage()
		if e != nil {
			return e
		}
		ts.reportStage(stageName)
		return nil
	}
	return errors.New("stage does not exist!")
}

func (ts *TraceStore) StartTime(stageName string) (time.Time, error) {
	if _, exists := ts.StageDurations[stageName]; !exists {
		return time.Time{}, errors.New("stage does not exist!")
	}
	return ts.StageDurations[stageName].StartTime, nil
}

func (ts *TraceStore) FinishTime(stageName string) (time.Time, error) {
	if _, exists := ts.StageDurations[stageName]; !exists {
		return time.Time{}, errors.New("stage does not exist!")
	}
	return ts.StageDurations[stageName].FinishTime, nil
}

func (ts *TraceStore) Duration(stageName string) (time.Duration, error) {
	finishTime, e := ts.FinishTime(stageName)
	if e != nil {
		return 0, e
	}
	startTime, e := ts.StartTime(stageName)
	if e != nil {
		return 0, e
	}

	return finishTime.Sub(startTime), nil
}

func (ts *TraceStore) GetIdentifier() string {
	return ts.Namespace + "/" + ts.Name
}
