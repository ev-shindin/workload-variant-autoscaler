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

	It("tracks the mean of what it sees, in both directions", func() {
		s := newBucketStore()
		s.ObserveRate("k", 4.0, now)
		s.ObserveRate("k", 9.0, now.Add(time.Minute))
		s.ObserveRate("k", 6.0, now.Add(2*time.Minute))

		rate, ok := s.Rate("k", now.Add(2*time.Minute))
		Expect(ok).To(BeTrue())
		// Between the samples, not pinned to the largest. Under backlog the server is
		// never idle, so every sample is a reading of the service rate — preferring
		// the maximum would only ratchet capacity upward and under-scale.
		Expect(rate).To(BeNumerically(">", 4.0))
		Expect(rate).To(BeNumerically("<", 9.0))
	})

	It("comes down when the workload gets heavier within a bucket", func() {
		s := newBucketStore()
		at := now
		for i := 0; i < 6; i++ { // calibrate at 10 req/s
			s.ObserveRate("k", 10.0, at)
			at = at.Add(30 * time.Second)
		}
		fast, _ := s.Rate("k", at)

		for i := 0; i < 12; i++ { // longer prompts: the same replica now serves 5/s
			s.ObserveRate("k", 5.0, at)
			at = at.Add(30 * time.Second)
		}
		slow, ok := s.Rate("k", at)

		Expect(ok).To(BeTrue())
		Expect(slow).To(BeNumerically("<", fast*0.8),
			"a running maximum would still be reporting the old rate here")
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

	It("holds an unrefreshed rate and expires it past the window", func() {
		s := newBucketStore()
		s.ObserveRate("k", 10.0, now)
		s.ObserveRate("k", 10.0, now)

		rate, ok := s.Rate("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(rate).To(BeNumerically("~", 10.0, 0.01))

		_, ok = s.Rate("k", now.Add(ServiceRateWindow+time.Second))
		Expect(ok).To(BeFalse(), "stale evidence is dropped rather than aged into a guess")
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

// siblingPod names the second replica wherever a test needs two of them.
const siblingPod = "pod-b"

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
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}
	}

	It("declines when the estimator is not enabled", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		_, _, _, ok := a.rateAnchoredK2(atLimit(), "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("reports the current occupancy on a first overload, before anything is learned", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		k2, _, src, ok := a.rateAnchoredK2(atLimit(), "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))
		Expect(k2).To(BeNumerically("~", float64(occupancy), 1),
			"saturation at 16% KV is representable, which the occupancy path cannot do")
	})

	It("gives every replica of the bucket the same learned ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		hot := atLimit()
		learn(a, hot)

		hotK2, _, hotSrc, ok := a.rateAnchoredK2(hot, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(hotSrc).To(Equal(k2SrcRateBacklog), "backlogged: at its limit this cycle")

		// An idle sibling: different pod, no queue, a quarter of the arrival rate.
		// It must report the SAME capacity, because capacity is a property of the
		// bucket. aggregateByVariant takes the median of per-replica capacities, so
		// anything load-dependent here would blend incommensurable numbers and could
		// lift variant capacity while a sibling queues.
		cold := atLimit()
		cold.PodName = siblingPod
		cold.QueueLength = 0
		cold.ArrivalRate = 2.0
		coldK2, _, coldSrc, ok := a.rateAnchoredK2(cold, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(coldSrc).To(Equal(k2SrcRateAnchored), "not at its limit: carrying the bucket's ceiling")
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
			k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold,
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
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}

		// Queue drained but arrivals still at the service rate: the replica is at its
		// limit, and the detector says so with no queue to go on.
		rm.QueueLength = 0
		rm.TokensInUse = 48_000
		k2, _, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog), "the arrivals path fired without a queue")
		// The lower occupancy must NOT become the new ceiling. With no queue it is
		// evidence the replica is keeping up, not evidence its limit has fallen —
		// recording it would ratchet capacity down on evidence of health.
		Expect(k2).To(BeNumerically("~", occupancy, 1))
	})

	It("uses completions as arrivals when there is no EPP and no queue", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}

		rm.QueueLength = 0
		rm.ArrivalRate = 0   // no EPP
		rm.RequestRate = 8.0 // completions == arrivals with no queue
		_, _, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		// RATE-now is only reachable here through the completions substitution: with
		// no queue and no EPP there is nothing else that could flag the limit.
		Expect(src).To(Equal(k2SrcRateBacklog))
	})

	It("declines for an idle replica in a bucket that has learned nothing", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.QueueLength = 0
		rm.ArrivalRate = 1.0
		_, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("declines for a cold replica with no occupancy", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.TokensInUse = 0 // just became ready, KV still empty
		_, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("does not let a shallow queue teach the bucket anything", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.QueueLength = 1 // arrival jitter, not a limit
		for i := 0; i < 5; i++ {
			_, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
			Expect(ok).To(BeFalse())
		}
	})

	It("floors the ceiling so a stalled replica cannot demand unbounded scale-up", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := atLimit()
		rm.TokensInUse = 100 // stalled with a deep queue and almost nothing resident
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
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
			k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
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
			_, _, _, _ = a.rateAnchoredK2(dec, "m", domain.RoleDecode, 1, k1, queueThreshold, now)
		}

		// A prefill replica of the same model on the same accelerator must not
		// inherit decode's limit.
		pre := atLimit()
		pre.PodName = "pod-p"
		pre.QueueLength = 0
		pre.ArrivalRate = 3
		_, _, _, ok := a.rateAnchoredK2(pre, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("never fabricates a limit for a prefill replica reporting no completions", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		// A disaggregated prefill pod can report zero completions even under load.
		rm := atLimit()
		rm.RequestRate = 0
		for i := 0; i < 5; i++ {
			_, _, src, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
			Expect(ok).To(BeTrue())
			// The queue is real, so the limit is measured from it — but no service
			// rate is ever learned, so nothing is inferred from a rate comparison.
			Expect(src).To(BeElementOf(k2SrcRateBacklog, k2SrcRateAnchored))
		}

		rm.QueueLength = 0
		rm.ArrivalRate = 1
		_, _, _, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
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

var _ = Describe("Rate-anchored k2 at the current operating point", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
		interval       = 15 * time.Second
	)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	// The numbers are Little's-law consistent, or the arithmetic below would be a
	// coincidence rather than the property under test: a replica serving mu = 8 req/s
	// with residence W = 6 s at 1250 tokens per request holds 8 x 6 x 1250 = 60_000
	// tokens. That is the occupancy at the limit, hence the ceiling.
	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           60_000,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               1.0,
			AvgITL:                0.02, // W = 1.0 + 250 x 0.02 = 6 s
		}
	}

	// Same arrivals, no queue, half the residence: what a replica looks like once
	// siblings have absorbed the backlog. Occupancy follows W down (8 x 3 x 1250).
	drained := func() domain.ReplicaMetrics {
		rm := atLimit()
		rm.QueueLength = 0
		rm.TokensInUse = 30_000
		rm.AvgTTFT = 0.5
		rm.AvgITL = 0.01 // W = 3 s
		return rm
	}

	// cycle mirrors production: freeze the bucket's operating point, then compute.
	cycle := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics, at time.Time) (int64, int64, k2Source, bool) {
		a.serviceRates.FreezeWork(at)
		return a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, at)
	}

	run := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics, from time.Time, n int) (int64, k2Source) {
		var k2 int64
		var src k2Source
		for i := 0; i < n; i++ {
			k2, _, src, _ = cycle(a, rm, from.Add(time.Duration(i)*interval))
		}
		return k2, src
	}

	It("does not move the capacity it just measured", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		k2, src := run(a, atLimit(), start, 3)

		// mu x W x tokensPerRequest equals the occupancy that set the ceiling, so
		// engaging the scaling changes nothing at the point it was calibrated.
		Expect(k2).To(Equal(int64(60_000)))
		Expect(src).To(Equal(k2SrcRateBacklog), "still at its limit")
	})

	It("holds utilization flat when contention falls away", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		hotK2, _ := run(a, atLimit(), start, 3)
		hot := atLimit()
		hotUtilization := float64(hot.TokensInUse) / float64(hotK2)

		// Two cycles: the operating point is published at the top of the next cycle,
		// so a change takes one cycle to reach every replica of the bucket.
		coldK2, coldSrc := run(a, drained(), start.Add(3*interval), 2)
		cold := drained()
		coldUtilization := float64(cold.TokensInUse) / float64(coldK2)

		Expect(coldSrc).To(Equal(k2SrcRateResidence))
		Expect(coldK2).To(BeNumerically("<", hotK2), "capacity follows the operating point down")
		// This is the whole fix: demand fell by half when the backlog cleared, and
		// capacity fell with it, so nothing reads as spare capacity and nothing is
		// shed. Round 1 held capacity flat here and sheds to one replica.
		Expect(coldUtilization).To(BeNumerically("~", hotUtilization, 0.01))
	})

	It("never scales capacity above the limit it measured", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		run(a, atLimit(), start, 3)

		// A deep queue inflates TTFT, so W reads high — that is queueing time, not
		// capacity. Letting it raise the bound would relax it exactly when the
		// replica is failing.
		queued := atLimit()
		queued.QueueLength = 140
		queued.AvgTTFT = 8.0 // W = 13 s, more than double the calibration point
		k2, _, src, ok := cycle(a, queued, start.Add(4*interval))

		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("<=", int64(60_000)))
		Expect(src).NotTo(Equal(k2SrcRateResidence))
	})

	It("gives every replica of a bucket the same capacity within a cycle", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		run(a, atLimit(), start, 3)
		run(a, drained(), start.Add(3*interval), 2)

		// Siblings report slightly different residences; they must still scale by one
		// number, because aggregateByVariant takes the MEDIAN of per-replica values.
		at := start.Add(5 * interval)
		a.serviceRates.FreezeWork(at)
		first := drained()
		second := drained()
		second.PodName = siblingPod
		second.AvgTTFT = 0.9
		second.AvgITL = 0.03 // a materially longer W than pod-a's

		k2a, _, _, _ := a.rateAnchoredK2(first, "m", "", 1, k1, queueThreshold, at)
		k2b, _, _, _ := a.rateAnchoredK2(second, "m", "", 1, k1, queueThreshold, at)
		Expect(k2b).To(Equal(k2a))
	})

	It("holds at the ceiling until an operating point has been published", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		// No FreezeWork call at all: nothing has been published, so there is nothing
		// to scale by and the estimator answers with the measured ceiling.
		var k2 int64
		var src k2Source
		for i := 0; i < 3; i++ {
			k2, _, src, _ = a.rateAnchoredK2(atLimit(), "m", "", 1, k1, queueThreshold,
				start.Add(time.Duration(i)*interval))
		}
		Expect(k2).To(Equal(int64(60_000)))
		Expect(src).NotTo(Equal(k2SrcRateResidence))
	})
})

var _ = Describe("Rate-anchored k2 operating point across siblings", func() {
	const (
		k1             = int64(320_000)
		queueThreshold = 5.0
		interval       = 15 * time.Second
	)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	atLimit := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			TokensInUse:           60_000,
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
			AvgTTFT:               1.0,
			AvgITL:                0.02, // W = 6 s, work = 7500 token-seconds
		}
	}

	It("averages the operating point over the cycle's replicas, whatever their order", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		for i := 0; i < MinServiceRateSamples+1; i++ {
			at := start.Add(time.Duration(i) * interval)
			a.serviceRates.FreezeWork(at)
			_, _, _, _ = a.rateAnchoredK2(atLimit(), "m", "", 1, k1, queueThreshold, at)
		}

		// One cycle, two replicas with materially different residences: 3 s and 1.2 s,
		// so 3750 and 1500 token-seconds. Whichever the loop reaches first, the bucket
		// must land on the mean — 2625 — and not on either endpoint.
		at := start.Add(3 * interval)
		a.serviceRates.FreezeWork(at)
		slow := atLimit()
		slow.QueueLength = 0
		slow.AvgTTFT, slow.AvgITL = 0.5, 0.01 // W = 3 s
		fast := slow
		fast.PodName = siblingPod
		fast.AvgTTFT, fast.AvgITL = 0.2, 0.004 // W = 1.2 s
		_, _, _, _ = a.rateAnchoredK2(slow, "m", "", 1, k1, queueThreshold, at)
		_, _, _, _ = a.rateAnchoredK2(fast, "m", "", 1, k1, queueThreshold, at)

		next := at.Add(interval)
		a.serviceRates.FreezeWork(next)
		k2, _, src, ok := a.rateAnchoredK2(slow, "m", "", 1, k1, queueThreshold, next)

		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateResidence))
		// mu x mean work = 8 x 2625, less the slow decay mu carries with age. Taking
		// the first replica alone would give 30_000 and the second alone 12_000 — and
		// which one it was would depend on the order the loop reached them.
		Expect(k2).To(BeNumerically("~", 21_000, 210)) // within 1%
	})
})

var _ = Describe("Rate-anchored k2 ceiling measurement", func() {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	It("measures the limit from the instantaneous reading, not the minute's peak", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			TokensInUse:           120_000, // max_over_time: the last minute's peak
			KvUsageInstant:        0.15,    // 60k: where it actually is now
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, 320_000, 5.0,
				now.Add(time.Duration(i)*15*time.Second))
		}

		// The ceiling is a running minimum, so feeding it a peak biases it high in the
		// one direction that costs replicas.
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, 320_000, 5.0, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("~", 60_000, 1))
	})

	It("falls back to the averaged reading when no instantaneous one is collected", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			TokensInUse:           120_000,
			TotalKvCapacityTokens: 400_000,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
		for i := 0; i < MinServiceRateSamples+1; i++ {
			_, _, _, _ = a.rateAnchoredK2(rm, "m", "", 1, 320_000, 5.0,
				now.Add(time.Duration(i)*15*time.Second))
		}
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", "", 1, 320_000, 5.0, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("~", 120_000, 1))
	})
})
