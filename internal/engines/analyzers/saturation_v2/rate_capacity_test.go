package saturation_v2

import (
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
		short := serviceRateKey("m", "H100", 1, 300, 300)
		long := serviceRateKey("m", "H100", 1, 1000, 300)
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
			_, _, _, _ = a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
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
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("declines when the estimator is not enabled", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		_, _, _, ok := a.rateAnchoredK2(saturatedReplica(), "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("reads 100% utilization for a replica at rate saturation", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()

		k2, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		// lambda == mu, so capacity equals current occupancy: demand/supply == 1
		// even though only 16% of KV is in use.
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1))
	})

	It("scales capacity by the distance to saturation", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm) // establishes mu = 8 req/s for this bucket

		// Same bucket, half the arrival rate and no backlog: twice the headroom.
		rm.QueueLength = 0
		rm.ArrivalRate = 4.0
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16*2, 1))
	})

	It("treats a backlogged replica as saturated when EPP provides no lambda", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.ArrivalRate = 0

		k2, src, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16, 1))
	})

	It("falls back to completions as lambda when there is no queue and no EPP", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm) // establishes mu = 8 req/s for this bucket

		rm.QueueLength = 0
		rm.ArrivalRate = 0
		rm.RequestRate = 4.0 // no queue, so completions are arrivals
		k2, src, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateNoEPP))
		Expect(k2).To(BeNumerically("~", float64(kvCapacity)*0.16*2, 1))
	})

	It("declines when idle and never calibrated", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.QueueLength = 0 // no backlog to fall back on
		rm.ArrivalRate = 0 // no EPP
		rm.RequestRate = 0 // and nothing completing
		_, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("prefers the EPP arrival rate once mu is established", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()

		// Before calibration the backlog itself is the only usable signal.
		_, src, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))

		// Once the bucket has enough qualifying observations, lambda wins.
		calibrate(a, rm)
		_, src, _, ok = a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateAnchored))
	})

	It("declines when the instantaneous KV reading is missing", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.KvUsageInstant = 0

		_, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse())
	})

	It("clamps the ratio so a collapsed arrival rate cannot inflate capacity", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		calibrate(a, rm)

		rm.QueueLength = 0
		rm.ArrivalRate = 0.0001 // mu/lambda would be 80000×
		k2, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically("<=", int64(float64(kvCapacity)*0.16*MaxRateRatio)))
	})

	It("floors the estimate so a stalled replica cannot drive capacity to zero", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := saturatedReplica()
		rm.RequestRate = 0.01
		rm.ArrivalRate = 50.0
		rm.KvUsageInstant = 0.01

		k2, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(k2).To(BeNumerically(">=", int64(float64(k1)*MinRateAnchoredFraction)))
	})
})

var _ = Describe("Rate-anchored k2 and the capacity store", func() {
	const (
		kvCapacity     = int64(400_000)
		queueThreshold = 5.0
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	replica := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			VariantName:           "v1",
			AcceleratorName:       "H100",
			QueueLength:           12,
			RequestRate:           8.0,
			ArrivalRate:           8.0,
			KvUsageInstant:        0.16,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("marks a saturated reading as a ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		_, _, ceiling, ok := a.rateAnchoredK2(replica(), "m", 1, int64(320_000), queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(ceiling).To(BeTrue(), "lambda >= mu means the replica demonstrated this capacity")
	})

	It("does not mark a below-saturation reading as a ceiling", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := replica()
		for i := 0; i < MinServiceRateSamples; i++ {
			_, _, _, _ = a.rateAnchoredK2(rm, "m", 1, int64(320_000), queueThreshold, now)
		}

		rm.QueueLength = 0
		rm.ArrivalRate = 2.0 // well below mu: this is headroom, not a ceiling
		_, _, ceiling, ok := a.rateAnchoredK2(rm, "m", 1, int64(320_000), queueThreshold, now)
		Expect(ok).To(BeTrue())
		Expect(ceiling).To(BeFalse())
	})
})

var _ = Describe("Rate-anchored k2 and transient queues", func() {
	const (
		kvCapacity     = int64(400_000)
		k1             = int64(320_000)
		queueThreshold = 5.0
	)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	// A replica comfortably keeping up: low occupancy, arrivals served as they come,
	// and a single request briefly waiting from arrival jitter. mu == lambda here is
	// flow conservation, not evidence of a ceiling, so nothing may be concluded.
	keepingUp := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			AcceleratorName:       "H100",
			QueueLength:           1,
			RequestRate:           3.0,
			ArrivalRate:           3.0,
			KvUsageInstant:        0.08,
			TotalKvCapacityTokens: kvCapacity,
			AvgInputTokens:        1000,
			AvgOutputTokens:       250,
		}
	}

	It("does not calibrate mu from a queue shallower than the threshold", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := keepingUp()
		for i := 0; i < 5; i++ {
			_, _, _, _ = a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		}

		_, _, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		Expect(ok).To(BeFalse(), "a one-deep queue is arrival jitter, not a ceiling")
	})

	It("does not declare saturation from a single deep-queue sample", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore(), withRateAnchoredK2(true))
		rm := keepingUp()
		rm.QueueLength = 6 // one qualifying observation, below MinServiceRateSamples

		_, src, _, ok := a.rateAnchoredK2(rm, "m", 1, k1, queueThreshold, now)
		// The backlog path still fires — a deep queue now is saturation now — but mu
		// is not yet established, so no ratio is derived from it.
		Expect(ok).To(BeTrue())
		Expect(src).To(Equal(k2SrcRateBacklog))
	})
})
