package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// These exercise computeReplicaCapacity rather than rateAnchoredK2 in isolation:
// what the capacity store learns, that nothing changes when the estimator is off,
// and the two behaviours the design exists for — holding capacity after a drain,
// and giving siblings a value the variant-level median can combine safely.
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

	// atLimit is queueing deep enough to calibrate, at 20% KV occupancy.
	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			VariantName:           variant,
			ModelID:               modelID,
			Namespace:             namespace,
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvCacheUsage:          0.20,
			TokensInUse:           int64(0.20 * float64(kvCapacity)),
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("learns the measured limit into the capacity store", func() {
		store := NewCapacityKnowledgeStore()
		a := NewSaturationAnalyzer(store, withRateAnchoredK2(true))
		cfg := scalingConfig()
		rm := atLimit()

		var rc *ReplicaCapacity
		for i := 0; i < MinServiceRateSamples+1; i++ {
			rc = a.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		}
		Expect(rc).NotTo(BeNil())
		Expect(rc.K2Priority).To(BeElementOf(k2SrcRateBacklog, k2SrcRateAnchored))

		rec := store.Get(namespace, modelID, variant)
		Expect(rec).NotTo(BeNil())
		// What the store learns is a limit measured under load, so it is the same
		// figure this cycle scaled on — no separate stored value is needed.
		Expect(rec.EffectiveCapacity).To(Equal(rc.EffectiveCapacity))
		Expect(rec.EffectiveCapacity).To(BeNumerically("<=", int64(0.8*float64(kvCapacity))))
	})

	It("is inert when the estimator is off", func() {
		cfg := scalingConfig()
		rm := atLimit()

		off := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		Expect(off.serviceRates).To(BeNil(), "guards the default of EnableRateAnchoredK2")
		Expect(off.arrivals).To(BeNil())

		baseline := off.computeReplicaCapacity(rm, cfg, modelID, namespace, 1, domain.RoleBoth)
		Expect(baseline).NotTo(BeNil())
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateAnchored))
		Expect(baseline.K2Priority).NotTo(Equal(k2SrcRateBacklog))

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

		peak := atLimit()
		peak.KvCacheUsage = 0.85
		peak.TokensInUse = int64(0.85 * float64(kvCapacity)) // 340k, above k1

		off := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		on := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_ = off.computeReplicaCapacity(peak, cfg, modelID, namespace, 1, domain.RoleBoth)
			_ = on.computeReplicaCapacity(peak, cfg, modelID, namespace, 1, domain.RoleBoth)
		}

		// Queue drained, occupancy collapsed — but arrivals are unchanged, still at
		// the measured service rate. Nothing about the load has improved.
		drained := peak
		drained.QueueLength = 0
		drained.KvCacheUsage = 0.16
		drained.TokensInUse = int64(0.16 * float64(kvCapacity)) // 64k
		drained.ArrivalRate = 8.0

		occupancyBased := off.computeReplicaCapacity(drained, cfg, modelID, namespace, 1, domain.RoleBoth)
		rateBased := on.computeReplicaCapacity(drained, cfg, modelID, namespace, 1, domain.RoleBoth)

		// The occupancy path answers from its inflated history and reports abundant
		// spare capacity: demand 64k against a capacity of k1. That is the shed.
		Expect(occupancyBased.EffectiveCapacity).To(BeNumerically(">", drained.TokensInUse*3),
			"documents the behaviour being replaced")
		Expect(occupancyBased.IsSaturated).To(BeFalse())

		// The rate path still holds the limit it measured, so utilization stays high
		// and nothing is shed.
		Expect(rateBased.EffectiveCapacity).To(BeNumerically("<=", drained.TokensInUse),
			"capacity tracks the measured limit, not a stale peak")
		Expect(rateBased.IsSaturated).To(BeTrue())
	})

	It("gives a backlogged replica and an idle sibling the same capacity", func() {
		cfg := scalingConfig()
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		hot := atLimit()
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_ = a.computeReplicaCapacity(hot, cfg, modelID, namespace, 1, domain.RoleBoth)
		}
		hotRC := a.computeReplicaCapacity(hot, cfg, modelID, namespace, 1, domain.RoleBoth)

		cold := atLimit()
		cold.PodName = "pod-b"
		cold.QueueLength = 0
		cold.ArrivalRate = 2.0
		coldRC := a.computeReplicaCapacity(cold, cfg, modelID, namespace, 1, domain.RoleBoth)

		// aggregateByVariant takes the MEDIAN of per-replica capacities. Equal values
		// make that median a no-op, so an idle sibling cannot lift variant capacity
		// while another replica queues — the regression that reintroduced shed-to-one.
		Expect(coldRC.EffectiveCapacity).To(Equal(hotRC.EffectiveCapacity))
		// The idle replica is not itself saturated: its own demand is what differs.
		Expect(coldRC.ReplicaDemand).To(BeNumerically("<", hotRC.ReplicaDemand))
	})
})
