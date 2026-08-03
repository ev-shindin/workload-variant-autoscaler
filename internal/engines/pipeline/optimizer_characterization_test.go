package pipeline

// Anchor-refactor characterization goldens.
//
// These tests capture the CURRENT (main) behavior of the sat-v2-only optimizer
// path as literal expected values ("goldens"). They exist to prove design
// invariant #7 of the anchor refactor: when exactly one analyzer votes
// (today's default config), the refactored pipeline must produce the same
// decisions as today. The same file rides unchanged onto the anchor-refactor
// branch as its ship gate.
//
// Every case here is single-analyzer, sat-v2-only (one NamedAnalyzerResult
// named domain.SaturationAnalyzerName). Because the expected values are
// captured from current code, every test in this file passes by construction
// against main — a red test here means the fixture is wrong, not that
// production code is buggy.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// goldenDecision is the subset of domain.VariantDecision fields the anchor
// refactor repoints and this branch freezes: target replica count,
// RequiredCapacity, SpareCapacity, and Utilization.
type goldenDecision struct {
	Replicas         int
	RequiredCapacity float64
	SpareCapacity    float64
	Utilization      float64
}

// expectDecisionSet asserts got matches want as a SET keyed by VariantName,
// never by slice order or slice equality: Optimize's per-decision content is
// deterministic but its output slice order is not (map iteration in
// buildDecisionsWithOptimizer, unstable sort in sortByRemainingDesc).
func expectDecisionSet(got []domain.VariantDecision, want map[string]goldenDecision) {
	gm := make(map[string]domain.VariantDecision, len(got))
	gotNames := make([]string, 0, len(got))
	for _, d := range got {
		gm[d.VariantName] = d
		gotNames = append(gotNames, d.VariantName)
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	Expect(gotNames).To(ConsistOf(wantNames), "decision-set variant names must match the golden")

	for name, w := range want {
		d := gm[name]
		Expect(d.TargetReplicas).To(Equal(w.Replicas), "variant %q: TargetReplicas", name)
		Expect(d.RequiredCapacity).To(BeNumerically("~", w.RequiredCapacity, 1e-9), "variant %q: RequiredCapacity", name)
		Expect(d.SpareCapacity).To(BeNumerically("~", w.SpareCapacity, 1e-9), "variant %q: SpareCapacity", name)
		Expect(d.Utilization).To(BeNumerically("~", w.Utilization, 1e-9), "variant %q: Utilization", name)
	}
}

// unlimitedConstraints emulates "no GPU limit" for GreedyByScoreOptimizer,
// which treats absent/empty constraints as zero (deny), not unlimited —
// mirrors the pattern in optimizer_equivalence_test.go.
func unlimitedConstraints(types ...string) []*ResourceConstraints {
	pools := map[string]ResourcePool{}
	for _, t := range types {
		pools[t] = ResourcePool{Limit: 1_000_000}
	}
	return []*ResourceConstraints{{Pools: pools}}
}

var _ = Describe("Anchor refactor characterization goldens (sat-v2-only)", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	Context("harness smoke test", func() {
		It("freezes a trivial no-op decision (proves the harness itself)", func() {
			// No demand (RequiredCapacity=0) and no spare (SpareCapacity=0): neither
			// optimizer has a reason to change the target. This exercises the
			// harness plumbing (fixture -> Optimize -> expectDecisionSet) without
			// depending on any allocation math.
			build := func() ModelScalingRequest {
				r := &domain.AnalyzerResult{
					ModelID:   "smoke",
					Namespace: "default",
					VariantCapacities: []domain.VariantCapacity{
						{VariantName: "v", AcceleratorName: "A100", Cost: 5.0, ReplicaCount: 2, PerReplicaCapacity: 10000, Utilization: 0.5},
					},
				}
				return withSatEntry(r, ModelScalingRequest{
					ModelID:   "smoke",
					Namespace: "default",
					Priority:  1,
					VariantStates: []domain.VariantReplicaState{
						{VariantName: "v", CurrentReplicas: 2, GPUsPerReplica: 1},
					},
				})
			}
			want := map[string]goldenDecision{
				"v": {Replicas: 2, RequiredCapacity: 0, SpareCapacity: 0, Utilization: 0.5},
			}

			expectDecisionSet(NewCostAwareOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, nil), want)
			expectDecisionSet(NewGreedyByScoreOptimizer().Optimize(ctx, []ModelScalingRequest{build()}, unlimitedConstraints("A100")), want)
		})
	})
})
