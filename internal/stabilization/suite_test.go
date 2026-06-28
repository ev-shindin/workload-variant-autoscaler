package stabilization

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

func TestStabilization(t *testing.T) {
	logging.NewTestLogger()
	RegisterFailHandler(Fail)
	RunSpecs(t, "Stabilization Suite")
}
