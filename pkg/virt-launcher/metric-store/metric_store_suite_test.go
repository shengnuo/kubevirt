package metricstore_test

import (
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestMetricStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MetricStore Suite")
}
