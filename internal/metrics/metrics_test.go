package metrics

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// resetMetrics clears package-level metric vars so each test starts fresh.
func resetMetrics() {
	replicaScalingTotal = nil
	desiredReplicas = nil
	currentReplicas = nil
	desiredRatio = nil
	saturationUtilization = nil
	spareCapacity = nil
	requiredCapacity = nil
	kvCacheTokensUsed = nil
	kvCacheTokensTotal = nil
	controllerInstance = ""
}

func gatherMetric(registry *prometheus.Registry, name string) *dto.MetricFamily {
	families, err := registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func gaugeValue(mf *dto.MetricFamily, labels map[string]string) (float64, bool) {
	for _, m := range mf.GetMetric() {
		match := true
		for k, v := range labels {
			found := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == k && lp.GetValue() == v {
					found = true
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match && m.GetGauge() != nil {
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

var _ = Describe("EmitSaturationMetrics", func() {

	var (
		registry *prometheus.Registry
		emitter  *MetricsEmitter
		ctx      context.Context
	)

	BeforeEach(func() {
		resetMetrics()
		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter = NewMetricsEmitter()
		ctx = context.Background()
	})

	It("should emit all saturation metrics with correct values", func() {
		err := emitter.EmitSaturationMetrics(ctx,
			"variant-a", "test-ns", "nvidia-a100",
			0.75, 0.25, 5000.0,
			100000, 200000,
		)
		Expect(err).NotTo(HaveOccurred())

		accelLabels := map[string]string{
			constants.LabelVariantName:     "variant-a",
			constants.LabelNamespace:       "test-ns",
			constants.LabelAcceleratorType: "nvidia-a100",
		}
		modelLabels := map[string]string{
			constants.LabelVariantName: "variant-a",
			constants.LabelNamespace:   "test-ns",
		}

		// saturation_utilization
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, accelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.75))

		// spare_capacity
		mf = gatherMetric(registry, constants.WVASpareCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, accelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.25))

		// required_capacity (model-level labels)
		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(5000.0))

		// kv_cache_tokens_used
		mf = gatherMetric(registry, constants.WVAKvCacheTokensUsed)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(100000.0))

		// kv_cache_tokens_total
		mf = gatherMetric(registry, constants.WVAKvCacheTokensTotal)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, modelLabels)
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(200000.0))
	})

	It("should include controller_instance label when env var is set", func() {
		// Re-init with controller instance set
		resetMetrics()
		Expect(os.Setenv(ControllerInstanceEnvVar, "controller-1")).To(Succeed())
		DeferCleanup(os.Unsetenv, ControllerInstanceEnvVar)

		registry = prometheus.NewRegistry()
		Expect(InitMetrics(registry)).To(Succeed())
		emitter = NewMetricsEmitter()

		err := emitter.EmitSaturationMetrics(ctx,
			"variant-b", "prod-ns", "nvidia-h100",
			0.90, 0.10, 10000.0,
			500000, 600000,
		)
		Expect(err).NotTo(HaveOccurred())

		// Verify controller_instance on accel-scoped metric
		mf := gatherMetric(registry, constants.WVASaturationUtilization)
		Expect(mf).NotTo(BeNil())
		val, ok := gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-b",
			constants.LabelNamespace:          "prod-ns",
			constants.LabelAcceleratorType:    "nvidia-h100",
			constants.LabelControllerInstance: "controller-1",
		})
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(0.90))

		// Verify controller_instance on model-scoped metric
		mf = gatherMetric(registry, constants.WVARequiredCapacity)
		Expect(mf).NotTo(BeNil())
		val, ok = gaugeValue(mf, map[string]string{
			constants.LabelVariantName:        "variant-b",
			constants.LabelNamespace:          "prod-ns",
			constants.LabelControllerInstance: "controller-1",
		})
		Expect(ok).To(BeTrue())
		Expect(val).To(Equal(10000.0))
	})

	It("should handle zero values correctly", func() {
		err := emitter.EmitSaturationMetrics(ctx,
			"variant-c", "ns", "amd-mi300x",
			0.0, 0.0, 0.0,
			0, 0,
		)
		Expect(err).NotTo(HaveOccurred())

		accelLabels := map[string]string{
			constants.LabelVariantName:     "variant-c",
			constants.LabelNamespace:       "ns",
			constants.LabelAcceleratorType: "amd-mi300x",
		}
		modelLabels := map[string]string{
			constants.LabelVariantName: "variant-c",
			constants.LabelNamespace:   "ns",
		}

		for _, metricName := range []string{
			constants.WVASaturationUtilization,
			constants.WVASpareCapacity,
		} {
			mf := gatherMetric(registry, metricName)
			Expect(mf).NotTo(BeNil(), "metric %s not found", metricName)
			val, ok := gaugeValue(mf, accelLabels)
			Expect(ok).To(BeTrue(), "gauge not found for %s", metricName)
			Expect(val).To(Equal(0.0), "expected 0 for %s", metricName)
		}

		for _, metricName := range []string{
			constants.WVARequiredCapacity,
			constants.WVAKvCacheTokensUsed,
			constants.WVAKvCacheTokensTotal,
		} {
			mf := gatherMetric(registry, metricName)
			Expect(mf).NotTo(BeNil(), "metric %s not found", metricName)
			val, ok := gaugeValue(mf, modelLabels)
			Expect(ok).To(BeTrue(), "gauge not found for %s", metricName)
			Expect(val).To(Equal(0.0), "expected 0 for %s", metricName)
		}
	})

	It("should return error when metrics are not initialized", func() {
		resetMetrics()
		uninitEmitter := NewMetricsEmitter()

		err := uninitEmitter.EmitSaturationMetrics(ctx,
			"v", "ns", "gpu",
			0.5, 0.5, 100.0,
			50, 100,
		)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not initialized"))
	})
})
