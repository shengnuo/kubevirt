package metricstore

import (
	"container/list"
	"math/rand"
	"strconv"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	metricexpo "kubevirt.io/kubevirt/pkg/virt-launcher/metric-store/metric-expo"
)

var _ = Describe("lifecycleDuration", func() {
	var (
		t        time.Time
		duration *lifecycleDuration
	)

	BeforeEach(func() {
		t = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
		duration = startTimestamp(t)
	})

	Describe("Basic functionalities", func() {
		Context("Startup timestamp", func() {
			It("Should contain a startup timestamp", func() {
				Expect(duration.startTime).To(Equal(t))
			})
		})

		Context("finish timestamp", func() {
			It("Should contain a finish timestamp", func() {
				t2 := time.Date(2002, time.January, 1, 0, 0, 0, 0, time.UTC)
				duration.finishTimestamp(t2)
				Expect(duration.finishTime).To(Equal(t2))
			})
		})
	})
})

type dummyNotifier struct{}

func (n *dummyNotifier) SendLifecycleMetrics(exporter metricexpo.MetricExporter) error {
	return nil
}

var _ = Describe("metricStore", func() {

	var (
		ms *metricStore
	)

	BeforeEach(func() {
		ms = &metricStore{
			namespace:          "namespace",
			name:               "name",
			uid:                "uid",
			pendingLifecycles:  list.New(),
			lifecycleDurations: make(map[string]*lifecycleDuration),
			myNotifier:         nil,
		}
	})

	Describe("Basic functionalities", func() {

		Context("newTimestamp", func() {
			It("Should create newTimestamp", func() {
				Expect(ms.newTimestamp("foo")).To(Succeed())
				Expect(ms.lifecycleDurations["foo"].startTime).ToNot(BeZero())
				Expect(ms.lifecycleDurations["foo"].finishTime).To(BeZero())
			})
		})

		Context("finishTimestamp", func() {
			It("should create finishTimestamp", func() {
				ms.newTimestamp("foo")
				Expect(ms.finishTimestamp("foo")).To(Succeed())
				Expect(ms.lifecycleDurations["foo"].finishTime).ToNot(BeZero())

			})

			It("Should throw an error if starttime is not found", func() {
				Expect(ms.finishTimestamp("foo")).To(MatchError("lifecycle does not exist!"))
			})

			It("Should set finish timestamp in pending without a Notifier", func() {
				ms.newTimestamp("foo")
				ms.finishTimestamp("foo")
				Expect(ms.pendingLifecycles.Len()).To(Equal(1))
				Expect(ms.pendingLifecycles.Front().Value).To(Equal("foo"))
			})

			It("Should not have timestamps in pending with a Notifier", func() {
				ms.newTimestamp("foo")
				ms.updateNotifier(&dummyNotifier{})
				ms.finishTimestamp("foo")
				Expect(ms.pendingLifecycles.Len()).To(BeZero())
				Expect(ms.lifecycleDurations).NotTo(HaveKey("foo"))
			})

			It("Should release timestamps in pending after updating a Notifier", func() {
				ms.newTimestamp("foo")
				ms.newTimestamp("bar")
				ms.finishTimestamp("foo")
				ms.finishTimestamp("bar")

				Expect(ms.pendingLifecycles.Len()).To(Equal(2))
				ms.updateNotifier(&dummyNotifier{})

				Expect(ms.pendingLifecycles.Len()).To(BeZero())
				Expect(ms.lifecycleDurations).To(BeEmpty())
			})

		})

	})

	Describe("Concurrency handling", func() {
		AssertConcurrencyFunc := func() {
			var wg sync.WaitGroup
			threads := 4
			notiferUpdater := rand.Intn(threads)

			wg.Add(threads)
			for t := 0; t < threads; t++ {
				go func(t, notiferUpdater int) {
					defer wg.Done()
					name := strconv.Itoa(t)
					ms.newTimestamp(name)
					if t == notiferUpdater {
						ms.updateNotifier(&dummyNotifier{})
					}
					delay := rand.Float64()*1000 + 500
					time.Sleep(time.Duration(delay) * time.Millisecond)
					ms.finishTimestamp(name)
				}(t, notiferUpdater)
			}
			wg.Wait()
		}

		It("Should handle concurrent operations", func() {
			Expect(AssertConcurrencyFunc).ToNot(Panic())
		})
	})
})
