package metricstore

import (
	"container/list"
	"errors"
	"sync"
	"time"

	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/metric-store/metric-expo"
)

type notifier interface {
	SendLifecycleMetrics(exporter metricexpo.MetricExporter) error
}

type lifecycleDuration struct {
	startTime  time.Time
	finishTime time.Time
}

func startTimestamp(startTime time.Time) *lifecycleDuration {
	return &lifecycleDuration{
		startTime: startTime,
	}
}

func (sd *lifecycleDuration) finishTimestamp(finishTime time.Time) {
	if sd.finishTime.IsZero() {
		sd.finishTime = finishTime
	}
}

type metricStore struct {
	lock               sync.RWMutex
	name               string
	uid                string
	namespace          string
	lifecycleDurations map[string]*lifecycleDuration
	pendingLifecycles  *list.List
	myNotifier         notifier
}

func (ms *metricStore) newTimestamp(lifecycleName string) {
	startTime := time.Now()
	ms.lock.Lock()
	defer ms.lock.Unlock()

	if _, exists := ms.lifecycleDurations[lifecycleName]; !exists {
		ms.lifecycleDurations[lifecycleName] = startTimestamp(startTime)
	}
}

func (ms *metricStore) reportLifecycle(lifecycleName string) {
	d, _ := ms.duration(lifecycleName)

	ms.myNotifier.SendLifecycleMetrics(metricexpo.MetricExporter{
		Namespace:     ms.namespace,
		Name:          ms.name,
		LifecycleName: lifecycleName,
		UID:           ms.uid,
		Duration:      d,
	})
	delete(ms.lifecycleDurations, lifecycleName)
}

func (ms *metricStore) updateNotifier(myNotifier notifier) {
	ms.lock.Lock()
	defer ms.lock.Unlock()

	ms.myNotifier = myNotifier

	for lifecycleName := ms.pendingLifecycles.Front(); lifecycleName != nil; lifecycleName = lifecycleName.Next() {
		name, _ := lifecycleName.Value.(string)
		ms.reportLifecycle(name)
	}
	ms.pendingLifecycles.Init()
}

func (ms *metricStore) finishTimestamp(lifecycleName string) error {
	finishTime := time.Now()

	ms.lock.Lock()
	defer ms.lock.Unlock()

	if v, exists := ms.lifecycleDurations[lifecycleName]; exists {
		v.finishTimestamp(finishTime)
		if ms.myNotifier != nil {
			ms.reportLifecycle(lifecycleName)
		} else {
			ms.pendingLifecycles.PushBack(lifecycleName)
		}
		return nil
	}
	return errors.New("lifecycle does not exist!")
}

func (ms *metricStore) startTime(lifecycleName string) (time.Time, error) {
	if _, exists := ms.lifecycleDurations[lifecycleName]; !exists {
		return time.Time{}, errors.New("lifecycle does not exist!")
	}
	return ms.lifecycleDurations[lifecycleName].startTime, nil
}

func (ms *metricStore) finishTime(lifecycleName string) (time.Time, error) {
	if _, exists := ms.lifecycleDurations[lifecycleName]; !exists {
		return time.Time{}, errors.New("lifecycle does not exist!")
	}
	return ms.lifecycleDurations[lifecycleName].finishTime, nil
}

func (ms *metricStore) duration(lifecycleName string) (time.Duration, error) {
	finishTime, e := ms.finishTime(lifecycleName)
	if e != nil {
		return 0, e
	}
	startTime, e := ms.startTime(lifecycleName)
	if e != nil {
		return 0, e
	}

	return finishTime.Sub(startTime), nil
}

var ms *metricStore
var once sync.Once
var shutdownDuration string

func InitMetricStore(namespace string, name string, uid string) {
	once.Do(func() {
		ms = &metricStore{
			namespace:          namespace,
			name:               name,
			uid:                uid,
			pendingLifecycles:  list.New(),
			lifecycleDurations: make(map[string]*lifecycleDuration),
			myNotifier:         nil,
		}
	})
}

func NewTimestamp(lifecycleName string) {
	ms.newTimestamp(lifecycleName)
}

func FinishTimestamp(lifecycleName string) error {
	return ms.finishTimestamp(lifecycleName)
}

func UpdateNotifier(myNotifier notifier) {
	ms.updateNotifier(myNotifier)
}
