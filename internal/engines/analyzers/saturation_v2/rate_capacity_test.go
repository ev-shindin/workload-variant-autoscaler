package saturation_v2

import (
	"math"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("Service rate store", func() {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	It("keeps the highest rate observed for a bucket", func() {
		s := newServiceRateStore()
		s.Observe("k", 4.0, now)
		// three observations, so the sample floor is satisfied
		s.Observe("k", 9.0, now.Add(time.Minute))
		s.Observe("k", 6.0, now.Add(2*time.Minute))

		rate, ok := s.Rate("k", now.Add(2*time.Minute))
		Expect(ok).To(BeTrue())
		// Slightly under 9 because the peak decays between observations; the
		// point is that the later, lower 6.0 did not replace it.
		Expect(rate).To(BeNumerically("~", 9.0, 0.2))
	})

	It("withholds an estimate until a second qualifying observation", func() {
		s := newServiceRateStore()
		s.Observe("k", 7.0, now)

		_, ok := s.Rate("k", now)
		Expect(ok).To(BeFalse(), "one observation cannot distinguish a ceiling from a slow interval")

		s.Observe("k", 7.0, now)
		_, ok = s.Rate("k", now)
		Expect(ok).To(BeTrue())
	})

	It("ignores non-positive rates", func() {
		s := newServiceRateStore()
		s.Observe("k", 0, now)
		s.Observe("k", -3, now)

		_, ok := s.Rate("k", now)
		Expect(ok).To(BeFalse())
	})

	It("reports nothing for an unknown bucket", func() {
		_, ok := newServiceRateStore().Rate("missing", now)
		Expect(ok).To(BeFalse())
	})

	It("decays an unrefreshed maximum", func() {
		s := newServiceRateStore()
		s.Observe("k", 10.0, now)
		s.Observe("k", 10.0, now)

		rate, ok := s.Rate("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(rate).To(BeNumerically("~", 10.0*ServiceRateDecayPerWindow, 0.01))
	})

	It("lets a fresh lower observation win over a decayed peak", func() {
		s := newServiceRateStore()
		s.Observe("k", 10.0, now)
		// After a full window the peak has decayed to 7.5; 8 is now the better estimate.
		s.Observe("k", 8.0, now.Add(ServiceRateWindow))

		rate, ok := s.Rate("k", now.Add(ServiceRateWindow))
		Expect(ok).To(BeTrue())
		Expect(rate).To(BeNumerically("~", 8.0, 0.01))
	})

	It("stops answering past the window", func() {
		s := newServiceRateStore()
		s.Observe("k", 10.0, now)
		s.Observe("k", 10.0, now)

		_, ok := s.Rate("k", now.Add(ServiceRateWindow+time.Second))
		Expect(ok).To(BeFalse())
	})

	It("evicts entries older than the timeout", func() {
		s := newServiceRateStore()
		s.Observe("fresh", 5.0, now)
		s.Observe("fresh", 5.0, now)
		s.Observe("stale", 5.0, now.Add(-2*time.Hour))

		Expect(s.EvictStale(time.Hour, now)).To(Equal(1))
		_, ok := s.Rate("fresh", now)
		Expect(ok).To(BeTrue())
	})

	It("separates buckets by input length", func() {
		short := serviceRateKey("m", "H100", "decode", 1, 300, 300)
		long := serviceRateKey("m", "H100", "decode", 1, 1000, 300)
		Expect(short).NotTo(Equal(long))
	})
})

var _ = Describe("Rate-anchored k2", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000) // 0.8 × kvCapacity
		queueThreshold = 5.0            // config.DefaultQueueLengthThreshold
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// calibrate drives enough qualifying observations to establish mu for the bucket.
	calibrate := func(a *SaturationAnalyzer, rm domain.ReplicaMetrics) {
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}
	}

	// saturatedReplica is queueing at 16% KV — the prefill-heavy regime the
	// occupancy-based estimator cannot express.
	saturatedReplica := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvUsageInstant:        0.16,
			TokensInUse:           int64(0.16 * float64(kvCapacity)),
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("declines when the estimator is not enabled", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		_, _, ok := a.rateAnchoredK2(saturatedReplica(), "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("reads 100% utilization from the backlog before mu is calibrated", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()

		k2, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		// No mu yet, so this is the backlog path — assert the source, otherwise the
		// numeric result is indistinguishable from the lambda path.
		Expect(src).To(Equal(k2SrcRateBacklog))
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1))
	})

	It("reads 100% utilization from lambda == mu with no queue", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm) // mu = 8 req/s

		// Arrivals exactly at the ceiling, nothing queued yet: this is the case the
		// ratio arithmetic exists for, and it must come from the lambda path.
		rm.QueueLength = 0
		rm.ArrivalRate = 8.0
		k2, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateAnchored))
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1),
			"capacity equals occupancy, so utilization reads 100% at 16% KV")
	})

	It("declines when the replica has headroom, leaving the occupancy path to answer", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm) // establishes mu = 8 req/s for this bucket

		// Same bucket, half the arrival rate and no backlog. A headroom-scaled
		// number is not a capacity — and would be blended into the variant median
		// alongside a backlogged sibling's real ceiling — so nothing is returned.
		rm.QueueLength = 0
		rm.ArrivalRate = 4.0
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("treats a backlogged replica as saturated when EPP provides no lambda", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.ArrivalRate = 0

		k2, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1))
	})

	It("uses completions as lambda when there is no queue and no EPP", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm) // establishes mu = 8 req/s for this bucket

		rm.QueueLength = 0
		rm.ArrivalRate = 0
		rm.RequestRate = 8.0 // no queue, so completions are arrivals; at the ceiling
		k2, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateNoEPP))
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1))
	})

	It("declines when idle and never calibrated", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.QueueLength = 0 // no backlog to fall back on
		rm.ArrivalRate = 0 // no EPP
		rm.RequestRate = 0 // and nothing completing
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("prefers the EPP arrival rate once mu is established", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()

		// Before calibration the backlog itself is the only usable signal.
		_, src, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))

		// Once the bucket has enough qualifying observations, lambda wins.
		calibrate(a, rm)
		_, src, ok = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateAnchored))
	})

	It("declines when the occupancy reading is missing", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.TokensInUse = 0

		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("declines rather than inflating capacity when the arrival rate collapses", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm)

		rm.QueueLength = 0
		rm.ArrivalRate = 0.0001 // mu/lambda would be 80000×
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("floors the estimate so a stalled replica cannot drive capacity to zero", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.RequestRate = 0.01
		rm.ArrivalRate = 50.0
		rm.TokensInUse = int64(0.01 * float64(kvCapacity))

		k2, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically(">=", int64(float64(k1)*MinRateAnchoredFraction)))
	})
})

var _ = Describe("Rate-anchored k2 across roles and load levels", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	backlogged := func(role string, rate float64) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-" + role,
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           rate,
			ArrivalRate:           rate,
			KvUsageInstant:        0.16,
			TokensInUse:           int64(0.16 * float64(kvCapacity)),
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("does not let one role calibrate another's ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		// A decode replica establishes mu = 20 req/s.
		dec := backlogged(domain.RoleDecode, 20)
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(dec, "m", domain.RoleDecode, 1, k1, queueThreshold, now)
		}

		// A prefill replica of the same model on the same accelerator must not
		// inherit it: prefill completes at a different rate for the same requests.
		pre := backlogged(domain.RolePrefill, 3)
		pre.QueueLength = 0
		_, src, ok := a.rateAnchoredK2(pre, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse(), "prefill has no calibration of its own yet")
		Expect(src).To(Equal(k2Source(0)))
	})

	It("calibrates each role independently", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		for _, role := range []string{domain.RolePrefill, domain.RoleDecode, domain.RoleBoth} {
			rm := backlogged(role, 10)
			for i := 0; i < MinServiceRateSamples; i++ {
				_, _, _ = a.rateAnchoredK2(rm, "m", role, 1, k1, queueThreshold, now)
			}
			rm.QueueLength = 0
			rm.ArrivalRate = 10 // still at the ceiling for this role
			k2, src, ok := a.rateAnchoredK2(rm, "m", role, 1, k1, queueThreshold, now)
			Expect(ok).To(BeTrue(), "role %s", role)
			Expect(src).To(Equal(k2SrcRateAnchored))
			Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1), "role %s", role)
		}
	})

	It("does not cut capacity below what a queue-free replica is visibly handling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		// Calibrated conservatively at 8 req/s during a rough patch.
		rm := backlogged(domain.RoleBoth, 8)
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", domain.RoleBoth, 1, k1, queueThreshold, now)
		}

		// Now running slightly past that rate with nothing queued: the replica is
		// keeping up, so capacity must not be reported below its occupancy.
		rm.QueueLength = 0
		rm.ArrivalRate = 12
		rm.RequestRate = 12
		k2, _, ok := a.rateAnchoredK2(rm, "m", domain.RoleBoth, 1, k1, queueThreshold, now.Add(time.Minute))
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically(">=", rm.TokensInUse),
			"utilization must not exceed 100% for a replica with an empty queue")
	})

	It("stays out of the way entirely when the replica has ample headroom", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := backlogged(domain.RoleBoth, 20)
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", domain.RoleBoth, 1, k1, queueThreshold, now)
		}

		// A quarter of the ceiling, and holding a healthy chunk of KV: the occupancy
		// path keeps k1 as the bound, and this estimator contributes nothing.
		rm.QueueLength = 0
		rm.ArrivalRate = 5
		rm.TokensInUse = int64(0.5 * float64(kvCapacity))
		_, _, ok := a.rateAnchoredK2(rm, "m", domain.RoleBoth, 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("Rate-anchored k2 with degenerate or missing signals", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	base := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               "pod-a",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvUsageInstant:        0.16,
			TokensInUse:           int64(0.16 * float64(kvCapacity)),
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("never calibrates a prefill replica that reports no completions", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		// A disaggregated prefill pod completes few or no generation tokens, so the
		// completion-rate metric can read zero even under a deep backlog.
		rm := base()
		rm.RequestRate = 0
		for i := 0; i < 5; i++ {
			_, src, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
			Expect(ok).To(BeTrue())
			// The backlog is real, so saturation is reported — but from the queue,
			// never from a fabricated ceiling.
			Expect(src).To(Equal(k2SrcRateBacklog))
		}

		// With the queue drained there is nothing left to conclude from.
		rm.QueueLength = 0
		_, _, ok := a.rateAnchoredK2(rm, "m", domain.RolePrefill, 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("declines a negative arrival rate rather than inverting the ratio", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := base()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}

		rm.QueueLength = 0
		rm.ArrivalRate = -5 // mis-scraped
		rm.RequestRate = 0
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("does not propagate a NaN arrival rate into capacity", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := base()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		}

		rm.QueueLength = 0
		rm.ArrivalRate = math.NaN()
		k2, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		// A NaN ratio is caught before the int64 conversion, so either the estimator
		// declines or it returns a sane positive token count — never garbage.
		if ok {
			Expect(k2).To(BeNumerically(">", 0))
			Expect(k2).To(BeNumerically("<=", k1))
		}
	})

	It("declines a cold replica with no occupancy and no calibration", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := base()
		rm.QueueLength = 0
		rm.TokensInUse = 0 // just became ready, KV still empty
		_, _, ok := a.rateAnchoredK2(rm, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("shares mu across replicas but stays silent for the idle one", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))

		// pod-a is backlogged and establishes the bucket's ceiling.
		hot := base()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _ = a.rateAnchoredK2(hot, "m", "", 1, k1, queueThreshold, now)
		}

		// pod-b, same variant and shape, idle at a quarter of the ceiling. It gets
		// the shared mu, and its own arrival history — not pod-a's.
		cold := base()
		cold.PodName = "pod-b"
		cold.QueueLength = 0
		cold.ArrivalRate = 2.0
		_, _, ok := a.rateAnchoredK2(cold, "m", "", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse(),
			"an idle replica must contribute nothing, or its headroom would inflate "+
				"the variant capacity median against a backlogged sibling's ceiling")
	})

	It("collapses onto one smoothing entry when replicas report no pod name", func() {
		// Documents a real limitation rather than asserting it is harmless: with
		// PodName empty the smoother falls back to the bucket key, so two such
		// replicas share one arrival average.
		s := newArrivalSmoother()
		first := s.Smooth("bucket", 4.0, 60, now)
		second := s.Smooth("bucket", 40.0, 60, now.Add(time.Second))
		Expect(first).To(Equal(4.0))
		Expect(second).To(BeNumerically("<", 40.0),
			"the second replica's rate is blended into the first's average")
	})
})
