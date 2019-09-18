package tracestore

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"kubevirt.io/client-go/log"
	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/trace-store/metric-expo"
)

type notifier interface {
	SendLifecycleMetrics(exporter metricexpo.MetricExporter) error
}

type stageDuration struct {
	startTime  time.Time
	finishTime time.Time
}

func startStage(startTime time.Time) *stageDuration {
	return &stageDuration{
		startTime: startTime,
	}
}

func (sd *stageDuration) finishStage(finishTime time.Time) error {
	if sd.finishTime.IsZero() {
		sd.finishTime = finishTime
	}
	return nil
}

type traceStore struct {
	lock           sync.RWMutex
	name           string
	uid            string
	namespace      string
	stageDurations map[string]*stageDuration
	pendingStages  *list.List
	myNotifier     notifier
}

func (ts *traceStore) newStage(stageName string) error {
	startTime := time.Now()
	ts.lock.Lock()
	defer ts.lock.Unlock()

	if _, exists := ts.stageDurations[stageName]; !exists {
		ts.stageDurations[stageName] = startStage(startTime)
	}
	return nil
}

func (ts *traceStore) reportStage(stageName string) {
	log.Log.Infof("reporting stage %s", stageName)
	d, _ := ts.duration(stageName)

	ts.myNotifier.SendLifecycleMetrics(metricexpo.MetricExporter{
		Namespace: ts.namespace,
		Name:      ts.name,
		StageName: stageName,
		UID:       ts.uid,
		Duration:  d,
	})
	delete(ts.stageDurations, stageName)
}

func (ts *traceStore) updateNotifier(myNotifier notifier) {
	ts.lock.Lock()
	defer ts.lock.Unlock()

	ts.myNotifier = myNotifier

	for stageName := ts.pendingStages.Front(); stageName != nil; stageName = stageName.Next() {
		name, _ := stageName.Value.(string)
		ts.reportStage(name)
	}
	ts.pendingStages.Init()
}

func (ts *traceStore) finishStage(stageName string) error {
	finishTime := time.Now()

	ts.lock.Lock()
	defer ts.lock.Unlock()

	if v, exists := ts.stageDurations[stageName]; exists {

		e := v.finishStage(finishTime)
		if e != nil {
			return e
		}
		if ts.myNotifier != nil {
			ts.reportStage(stageName)
		} else {
			ts.pendingStages.PushBack(stageName)
		}
		return nil
	}
	return errors.New("stage does not exist!")
}

func (ts *traceStore) startTime(stageName string) (time.Time, error) {
	if _, exists := ts.stageDurations[stageName]; !exists {
		return time.Time{}, errors.New("stage does not exist!")
	}
	return ts.stageDurations[stageName].startTime, nil
}

func (ts *traceStore) finishTime(stageName string) (time.Time, error) {
	if _, exists := ts.stageDurations[stageName]; !exists {
		return time.Time{}, errors.New("stage does not exist!")
	}
	return ts.stageDurations[stageName].finishTime, nil
}

func (ts *traceStore) duration(stageName string) (time.Duration, error) {
	finishTime, e := ts.finishTime(stageName)
	if e != nil {
		return 0, e
	}
	startTime, e := ts.startTime(stageName)
	if e != nil {
		return 0, e
	}

	return finishTime.Sub(startTime), nil
}

var ts *traceStore
var once sync.Once
var shutdownDuration string

func InitTraceStore(namespace string, name string, uid string) {
	once.Do(func() {
		ts = &traceStore{
			namespace:      namespace,
			name:           name,
			uid:            uid,
			pendingStages:  list.New(),
			stageDurations: make(map[string]*stageDuration),
			myNotifier:     nil,
		}
	})
}

func NewStage(stageName string) error {
	return ts.newStage(stageName)
}

func FinishStage(stageName string) error {
	return ts.finishStage(stageName)
}

func UpdateNotifier(myNotifier notifier) {
	ts.updateNotifier(myNotifier)
}
