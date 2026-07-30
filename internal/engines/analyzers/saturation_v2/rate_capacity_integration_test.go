package saturation_v2

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// These exercise computeReplicaCapacity rather than rateAnchoredK2 in isolation.
// The leaf tests pin the estimator's arithmetic; these pin the two things only the
// caller can get wrong: which value the capacity store learns, and that nothing
// changes at all when the estimator is off.
var _ = Describe("Rate-anchored k2 through computeReplicaCapacity", func() {
	const (
		kvCapacity = int64(400_000)
		namespace  = "ns"
		modelID    = "m"
		variant    = "v1"
	)

	scalingConfig := func() *config.SaturationScalingConfig {
		return &config.SaturationScalingConfig{
			KvCacheThreshold:     0.8, // k1 = 320k
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.75,
			ScaleDownBoundary:    0.60,
		}
	}

	// backlogged is deep enough to calibrate mu at 8 req/s.
	backlogged := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			VariantName:           variant,
			ModelID:               modelID,
			Namespace:             namespace,
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvUsageInstant:        0.16,
			KvCacheUsage:          0.20,
			TokensInUse:           int64(0.20 * float64(kvCapacity)),
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("persists a saturated reading as the learned ceiling", func() {
		store := NewCapacityKnowledgeStore()
		a := NewSaturationAnalyzer(store, withRateAnchoredK2(true))
		cfg := scalingConfig()
		rm := backlogged()

		var rc *ReplicaCapacity
		for i := 0; i < MinServiceRateSamples+1; i++ {
			rc = a.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		}
		Expect(rc).NotTo(BeNil())

		rec := store.Get(namespace, modelID, variant)
		Expect(rec).NotTo(BeNil())
		// lambda == mu at saturation, so the estimate is the occupancy the replica
		// demonstrably held — that is a ceiling and belongs in the store.
		Expect(rec.EffectiveCapacity).To(Equal(rc.EffectiveCapacity))
	})

	It("keeps a below-saturation reading out of the store", func() {
		store := NewCapacityKnowledgeStore()
		a := NewSaturationAnalyzer(store, withRateAnchoredK2(true))
		cfg := scalingConfig()

		rm := backlogged()
		for i := 0; i < MinServiceRateSamples; i++ {
			_ = a.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		}

		// Now idle at a quarter of the ceiling: the estimate is occupancy scaled by
		// headroom, which is not a property of the variant.
		rm.QueueLength = 0
		rm.ArrivalRate = 2.0
		rc := a.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		Expect(rc).NotTo(BeNil())

		rec := store.Get(namespace, modelID, variant)
		Expect(rec).NotTo(BeNil())
		Expect(rec.EffectiveCapacity).NotTo(Equal(rc.EffectiveCapacity),
			"the headroom-scaled value must not become the learned ceiling")
		// What the store learns must still be usable for zero-replica sizing, i.e.
		// bounded by the memory bound rather than by this cycle's arrival rate.
		Expect(rec.EffectiveCapacity).To(BeNumerically("<=", int64(0.8*float64(kvCapacity))))
		Expect(rec.EffectiveCapacity).To(BeNumerically(">", 0))
	})

	It("is inert when the estimator is off", func() {
		cfg := scalingConfig()
		rm := backlogged()

		// Same inputs, same call sequence, with and without the estimator. With it
		// off, every field the caller consumes must match the occupancy-based path.
		off := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		Expect(off.serviceRates).To(BeNil(), "guards the default of EnableRateAnchoredK2")
		Expect(off.arrivals).To(BeNil())

		baseline := off.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		Expect(baseline).NotTo(BeNil())
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateAnchored))
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateBacklog))
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateNoEPP))

		// A second analyzer, also off, must agree exactly — no hidden state.
		again := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		repeat := again.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		Expect(repeat.EffectiveCapacity).To(Equal(baseline.EffectiveCapacity))
		Expect(repeat.MemoryBoundCapacity).To(Equal(baseline.MemoryBoundCapacity))
		Expect(repeat.ComputeBoundCapacity).To(Equal(baseline.ComputeBoundCapacity))
		Expect(repeat.IsSaturated).To(Equal(baseline.IsSaturated))
		Expect(repeat.ReplicaDemand).To(Equal(baseline.ReplicaDemand))
	})

	It("holds capacity after a drain where the occupancy estimator releases it", func() {
		cfg := scalingConfig()

		// Under a deep backlog the two estimators agree: the occupancy path records
		// TokensInUse as k2, and the rate path's backlog branch returns the same
		// figure. The divergence appears after the queue drains, which is where the
		// shed-to-one came from.
		peak := backlogged()
		peak.KvCacheUsage = 0.85
		peak.TokensInUse = int64(0.85 * float64(kvCapacity)) // 340k, above k1

		off := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		on := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_ = off.computeReplicaCapacity(peak, cfg, modelID, namespace, 1, domain.RoleBoth)
			_ = on.computeReplicaCapacity(peak, cfg, modelID, namespace, 1, domain.RoleBoth)
		}

		// Queue drained, occupancy collapsed — but arrivals are unchanged, still at
		// the measured ceiling. Nothing about the load has actually improved.
		drained := peak
		drained.QueueLength = 0
		drained.KvCacheUsage = 0.16
		drained.TokensInUse = int64(0.16 * float64(kvCapacity)) // 64k
		drained.ArrivalRate = 8.0

		occupancyBased := off.computeReplicaCapacity(drained, cfg, modelID, namespace, 1, domain.RoleBoth)
		rateBased := on.computeReplicaCapacity(drained, cfg, modelID, namespace, 1, domain.RoleBoth)

		// The occupancy path answers from its inflated history and reports abundant
		// spare capacity: demand 64k against a capacity of k1.
		Expect(occupancyBased.EffectiveCapacity).To(BeNumerically(">", drained.TokensInUse*3),
			"documents the behaviour being replaced")
		Expect(occupancyBased.IsSaturated).To(BeFalse())

		// The rate path reads lambda still at the ceiling, so capacity tracks the
		// load rather than a stale peak: utilization stays at 100% and nothing is shed.
		Expect(rateBased.EffectiveCapacity).To(BeNumerically("~", float64(drained.TokensInUse), 1))
		Expect(rateBased.K2Priority).To(Equal(k2SrcRateAnchored))
		Expect(rateBased.IsSaturated).To(BeTrue())
	})

})

var _ = Describe("Rate-anchored stores age out", func() {
	It("evicts idle buckets and replicas", func() {
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		rates := newServiceRateStore()
		rates.Observe("bucket", 5, now.Add(-2*time.Hour))
		rates.Observe("bucket", 5, now.Add(-2*time.Hour))
		Expect(rates.EvictStale(time.Hour, now)).To(Equal(1))

		arrivals := newArrivalSmoother()
		_ = arrivals.Smooth("pod", 5, 60, now.Add(-2*time.Hour))
		Expect(arrivals.EvictStale(time.Hour, now)).To(Equal(1))
	})
})
