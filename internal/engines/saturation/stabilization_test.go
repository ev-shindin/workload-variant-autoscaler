package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/stabilization"
)

func newStabilizationEngine(enabled bool) *Engine {
	cfg := config.NewTestConfig()
	cfg.UpdateSaturationConfig(map[string]config.SaturationScalingConfig{
		"default": {EnableStabilization: enabled},
	})
	return &Engine{stabilizer: stabilization.New(), Config: cfg}
}

func scaleDecision(current, target int) domain.VariantDecision {
	d := domain.VariantDecision{
		Namespace:       "ns",
		ModelID:         "model",
		VariantName:     "v",
		CurrentReplicas: current,
		TargetReplicas:  target,
	}
	d.SetDecisionReason(d.ActionForTarget(), domain.DecisionReasonV2, "V2 (optimizer: cost-aware)")
	return d
}

var _ = Describe("applyStabilization", func() {
	It("is a no-op when EnableStabilization is false", func() {
		e := newStabilizationEngine(false)
		decisions := []domain.VariantDecision{scaleDecision(2, 10)}
		e.applyStabilization(context.Background(), decisions)
		Expect(decisions[0].TargetReplicas).To(Equal(10), "raw optimizer target must pass through unchanged")
	})

	It("damps the target and recomputes the action when enabled", func() {
		e := newStabilizationEngine(true)
		// Default scale-up rate caps a 2->10 spike to max(+4 pods, +100%) = 6.
		decisions := []domain.VariantDecision{scaleDecision(2, 10)}
		e.applyStabilization(context.Background(), decisions)
		Expect(decisions[0].TargetReplicas).To(Equal(6))
		Expect(decisions[0].Action).To(Equal(domain.ActionScaleUp))
	})

	It("leaves the decision untouched when stabilization does not change the target", func() {
		e := newStabilizationEngine(true)
		// current=2, desired=3 is within the default +4-pods budget, so it passes through.
		d := scaleDecision(2, 3)
		wantReason := d.Reason()
		decisions := []domain.VariantDecision{d}
		e.applyStabilization(context.Background(), decisions)
		Expect(decisions[0].TargetReplicas).To(Equal(3), "target passes through when the stabilizer agrees")
		Expect(decisions[0].Reason()).To(Equal(wantReason), "reason is not rewritten when no retarget occurs")
	})
})

var _ = Describe("stabilizationKey", func() {
	It("omits the role for non-disaggregated variants", func() {
		Expect(stabilizationKey(&domain.VariantDecision{Namespace: "ns", ModelID: "m", VariantName: "v"})).To(Equal("ns/m/v"))
	})

	It("includes the role for disaggregated variants", func() {
		Expect(stabilizationKey(&domain.VariantDecision{Namespace: "ns", ModelID: "m", VariantName: "v", Role: "prefill"})).To(Equal("ns/m/v/prefill"))
	})
})

var _ = Describe("retargetDecision", func() {
	It("recomputes the action from the new target", func() {
		d := scaleDecision(5, 10) // initially scale-up
		d.TargetReplicas = 3
		retargetDecision(&d)
		Expect(d.Action).To(Equal(domain.ActionScaleDown))
	})

	It("falls back to the V2 reason category when unset", func() {
		d := &domain.VariantDecision{CurrentReplicas: 5, TargetReplicas: 3}
		retargetDecision(d)
		Expect(d.ReasonCategory()).To(Equal(domain.DecisionReasonV2))
		Expect(d.Action).To(Equal(domain.ActionScaleDown))
	})
})
