package saturation_v2

import (
	"fmt"
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("Bucket store — service rate", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("keeps the highest rate observed", func() {
		s := newBucketStore()
		s.ObserveRate("k", 4.0, now)
		s.ObserveRate("k", 9.0, now.Add(time.Minute))
		s.ObserveRate("k", 6.0, now.Add(2*time.Minute))

		rate, ok := s.Rate("k", now.Add(2*time.Minute))
		Expect(ok).To(BeTrue())
		// Slightly under 9 because the peak decays between observations; the point
		// is that the later, lower 6.0 did not replace it.
		Expect(rate).To(BeNumerically("~", 9.0, 0.2))
	})

	It("withholds an estimate until a second observation", func() {
		s := newBucketStore()
		s.ObserveRate("k", 7.0, now)
		_, ok := s.Rate("k", now)
		Expect(ok).To(BeFalse(), "one observation cannot distinguish a limit from a slow interval")

		s.ObserveRate("k", 7.0, now)
		_, ok = s.Rate("k", now)
		Expect(ok).To(BeTrue())
	})

	It("ignores non-positive rates", func() {
		s := newBucketStore()
		s.ObserveRate("k", 0, now)
		s.ObserveRate("k", -3, now)
		_, ok := s.Rate("k", now)
		Expect(ok).To(BeFalse())
	})

	It("decays an unrefreshed rate and expires it past the window", func() {
		s := newBucketStore()
		s.ObserveRate("k", 10.0, now)
		s.ObserveRate("k", 10.0, now)

		rate, ok := s.Rate("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(rate).To(BeNumerically("~", 10.0*ServiceRateDecayPerWindow, 0.01))

		_, ok = s.Rate("k", now.Add(ServiceRateWindow+time.Second))
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("Bucket store — token ceiling", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("keeps the lowest occupancy at which a limit was seen", func() {
		s := newBucketStore()
		s.ObserveCeiling("k", 90_000, now)
		s.ObserveCeiling("k", 64_000, now)
		s.ObserveCeiling("k", 80_000, now)

		c, ok := s.Ceiling("k", now)
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 64_000, 1),
			"a running minimum: the conservative reading of capacity")
	})

	It("relaxes upward when no fresh limit is observed", func() {
		s := newBucketStore()
		s.ObserveCeiling("k", 64_000, now)

		c, ok := s.Ceiling("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 64_000*CeilingRelaxPerWindow, 1),
			"a single pessimistic measurement must not cap the bucket forever")
	})

	It("lets a fresh higher measurement win over a relaxed one", func() {
		s := newBucketStore()
		s.ObserveCeiling("k", 64_000, now)
		s.ObserveCeiling("k", 120_000, now.Add(ServiceRateWindow))

		c, ok := s.Ceiling("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(c).To(BeNumerically("~", 80_000, 1), "64k relaxed to 80k, still below 120k")
	})

	It("expires past the window and evicts with the rate", func() {
		s := newBucketStore()
		s.ObserveCeiling("k", 64_000, now)
		_, ok := s.Ceiling("k", now.Add(ServiceRateWindow+time.Second))
		Expect(ok).To(BeFalse())

		Expect(s.EvictStale(time.Hour, now.Add(2*time.Hour))).To(Equal(1))
	})

	It("separates buckets by role and by input length", func() {
		decode := serviceRateKey("m", "H100", domain.RoleDecode, 1, 1000, 250)
		prefill := serviceRateKey("m", "H100", domain.RolePrefill, 1, 1000, 250)
		shortIn := serviceRateKey("m", "H100", domain.RoleDecode, 1, 300, 250)
		Expect(decode).NotTo(Equal(prefill))
		Expect(decode).NotTo(Equal(shortIn))
	})
})

var _ = Describe("Arrival smoothing over the residence time", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("returns the first sample unchanged", func() {
		Expect(newArrivalSmoother().Smooth("pod", 10.0, 60, now)).To(Equal(10.0))
	})

	It("lags a step change instead of following it", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 60, now)
		got := s.Smooth("pod", 16.0, 60, now.Add(15*time.Second))
		Expect(got).To(BeNumerically(">", 4.0))
		Expect(got).To(BeNumerically("<", 16.0))
	})

	It("converges once a few time constants have passed", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 10, now)
		got := 0.0
		for i := 1; i <= 10; i++ {
			got = s.Smooth("pod", 16.0, 10, now.Add(time.Duration(i)*10*time.Second))
		}
		Expect(got).To(BeNumerically("~", 16.0, 0.5))
	})

	It("discards a stale average rather than blending it", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 10, now)
		Expect(s.Smooth("pod", 16.0, 10, now.Add(time.Hour))).To(Equal(16.0))
	})

	It("passes through when the residence estimate is unavailable", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("pod", 4.0, 0, now)
		Expect(s.Smooth("pod", 16.0, 0, now.Add(time.Second))).To(Equal(16.0))
	})

	It("keeps replicas separate and evicts the absent", func() {
		s := newArrivalSmoother()
		_ = s.Smooth("a", 4.0, 60, now)
		Expect(s.Smooth("b", 20.0, 60, now)).To(Equal(20.0))
		_ = s.Smooth("gone", 4.0, 60, now.Add(-2*time.Hour))
		Expect(s.EvictStale(time.Hour, now)).To(Equal(1))
	})
})

var _ = Describe("Residence estimate", func() {
	It("is time to first token plus one ITL per output token", func() {
		Expect(residenceSeconds(domain.ReplicaMetrics{
			AvgTTFT: 2.0, AvgITL: 0.02, AvgOutputTokens: 250,
		})).To(BeNumerically("~", 2.0+250*0.02, 0.001))
	})

	It("declines without latency data", func() {
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgOutputTokens: 250})).To(BeZero())
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgITL: 0.02})).To(BeZero())
	})

	It("bounds an implausible reading", func() {
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgITL: 1e-7, AvgOutputTokens: 1})).
			To(Equal(MinResidenceSeconds))
		Expect(residenceSeconds(domain.ReplicaMetrics{AvgTTFT: 1e6, AvgITL: 1, AvgOutputTokens: 1})).
			To(Equal(MaxResidenceSeconds))
	})
})

var _ = Describe("Rate-anchored k2", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
		occupancy      = int64(64_000) // 16% of KV — the prefill-heavy regime
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           occupancy,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	// learn drives enough cycles to establish both the service rate and the ceiling.
	learn := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics) {
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}
	}

	It("declines when the estimator is not enabled", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		_, _, ok := a.rateAnchoredK2(atLimit(), "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("reports the current occupancy on a first overload, before anything is learned", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		k2, src, ok := a.rateAnchoredK2(atLimit(), "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))
		Expect(k2).To(BeNumerically("~", float64(occupancy), 1),
			"saturation at 16% KV is representable, which the occupancy path cannot do")
	})

	It("gives every replica of the bucket the same learned ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		hot := atLimit()
		learn(a, hot)

		hotK2, hotSrc, ok := a.rateAnchoredK2(hot, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(hotSrc).To(Equal(k2SrcRateAnchored))

		// An idle sibling: different pod, no queue, a quarter of the arrival rate.
		// It must report the SAME capacity, because capacity is a property of the
		// bucket. aggregateByVariant takes the median of per-replica capacities, so
		// anything load-dependent here would blend incommensurable numbers and could
		// lift variant capacity while a sibling queues.
		cold := atLimit()
		cold.PodName = "pod-b"
		cold.QueueLength = 0
		cold.ArrivalRate = 2.0
		coldK2, coldSrc, ok := a.rateAnchoredK2(cold, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(coldSrc).To(Equal(k2SrcRateAnchored))
		Expect(coldK2).To(Equal(hotK2), "the median across replicas must be a no-op")
	})

	It("returns the same value cycle after cycle for unchanged input", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		learn(a, rm)

		// Arrival rate wobbles around the service rate; capacity must not.
		seen := make([]int64, 0, 5)
		for i, lambda := range []float64{8.0, 7.2, 8.6, 7.9, 8.3} {
			rm.ArrivalRate = lambda
			rm.QueueLength = 0
			k2, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold,
				now.Add(time.Duration(i)*15*time.Second))
			Expect(ok).To(BeTrue())
			seen = append(seen, k2)
		}
		// lambda swings +-9% here. The only permitted movement is the ceiling's slow
		// relaxation with age (CeilingRelaxPerWindow over ServiceRateWindow), which
		// over a minute is a fraction of a percent — not a per-cycle response to load.
		for _, v := range seen {
			Expect(v).To(BeNumerically("~", float64(seen[0]), float64(seen[0])*0.01),
				"a capacity that moved with lambda each cycle is an oscillation waiting to happen")
		}
	})

	It("detects the limit from arrivals reaching the service rate, with no queue", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}

		// Queue drained but arrivals still at the ceiling, and occupancy lower than
		// what was measured under backlog: the lower reading is now the ceiling.
		rm.QueueLength = 0
		rm.TokensInUse = 48_000
		k2, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateAnchored))
		Expect(k2).To(BeNumerically("~", 48_000, 1))
	})

	It("uses completions as arrivals when there is no EPP and no queue", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}

		rm.QueueLength = 0
		rm.ArrivalRate = 0   // no EPP
		rm.RequestRate = 8.0 // completions == arrivals with no queue
		_, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateAnchored))
	})

	It("declines for an idle replica in a bucket that has learned nothing", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.QueueLength = 0
		rm.ArrivalRate = 1.0
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("declines for a cold replica with no occupancy", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.TokensInUse = 0 // just became ready, KV still empty
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("does not let a shallow queue teach the bucket anything", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.QueueLength = 1 // arrival jitter, not a limit
		for i := 0; i < 5; i++ {
			_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
			Expect(ok).To(BeFalse())
		}
	})

	It("floors the ceiling so a stalled replica cannot demand unbounded scale-up", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.TokensInUse = 100 // stalled with a deep queue and almost nothing resident
		k2, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically(">=", int64(float64(k1)*MinRateAnchoredFraction)))
	})

	It("survives negative and NaN arrival rates", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		learn(a, rm)

		for _, bad := range []float64{-5, math.NaN(), math.Inf(1)} {
			rm.QueueLength = 0
			rm.ArrivalRate = bad
			rm.RequestRate = 0
			k2, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
			if ok {
				Expect(k2).To(BeNumerically(">", 0))
			}
		}
	})

	It("keeps roles apart", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		dec := atLimit()
		dec.RequestRate, dec.ArrivalRate = 20, 20
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_, _, _ = a.rateAnchoredK2(dec, "m", domain.RoleDecode, 1, k1, queueThreshold, now)
		}

		// A prefill replica of the same model on the same accelerator must not
		// inherit decode's limit.
		pre := atLimit()
		pre.PodName = "pod-p"
		pre.QueueLength = 0
		pre.ArrivalRate = 3
		_, _, ok := a.rateAnchoredK2(pre, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("never fabricates a limit for a prefill replica reporting no completions", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		// A disaggregated prefill pod can report zero completions even under load.
		rm := atLimit()
		rm.RequestRate = 0
		for i := 0; i < 5; i++ {
			_, src, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
			Expect(ok).To(BeTrue())
			// The queue is real, so the limit is measured from it — but no service
			// rate is ever learned, so nothing is inferred from a rate comparison.
			Expect(src).To(BeElementOf(k2SrcRateBacklog, k2SrcRateAnchored))
		}

		rm.QueueLength = 0
		rm.ArrivalRate = 1
		_, _, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue(), "the ceiling measured under backlog still applies")
	})
})

var _ = Describe("Bucket store — bounded growth", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("prunes stale buckets when a new one is inserted", func() {
		s := newBucketStore()
		// Fill past the prune threshold with buckets nobody has touched in a day.
		old := now.Add(-2 * HistoryEvictionTimeout)
		for i := 0; i < BucketPruneThreshold; i++ {
			s.ObserveCeiling(fmt.Sprintf("stale-%d", i), 1000, old)
		}
		Expect(s.entries).To(HaveLen(BucketPruneThreshold))

		s.ObserveCeiling("fresh", 2000, now)
		Expect(s.entries).To(HaveLen(1), "the stale buckets went with the insert")
		_, ok := s.Ceiling("fresh", now)
		Expect(ok).To(BeTrue())
	})

	It("keeps buckets that are still in use", func() {
		s := newBucketStore()
		for i := 0; i < BucketPruneThreshold; i++ {
			s.ObserveCeiling(fmt.Sprintf("live-%d", i), 1000, now)
		}
		s.ObserveCeiling("one-more", 1000, now)
		Expect(s.entries).To(HaveLen(BucketPruneThreshold + 1))
	})

	It("prunes per-pod arrival entries the same way", func() {
		s := newArrivalSmoother()
		old := now.Add(-2 * HistoryEvictionTimeout)
		for i := 0; i < BucketPruneThreshold; i++ {
			_ = s.Smooth(fmt.Sprintf("gone-%d", i), 4, 60, old)
		}
		_ = s.Smooth("current", 4, 60, now)
		Expect(s.entries).To(HaveLen(1))
	})
})
