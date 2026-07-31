package saturation_v2

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Rate-anchored compute capacity.
//
// The occupancy-based estimator records `tokensInUse` at the moment a replica was
// seen queueing and calls that the compute bound. That is a KV *stock* standing in
// for a *rate* limit, and on a prefill-heavy workload the two are unrelated: the
// engine exhausts prompt-token throughput and queues while KV occupancy is still
// low, so the estimate carries no information about the binding constraint.
//
// This estimator splits the problem in two:
//
//	detector:    rates decide WHEN a replica is at its limit
//	measurement: tokens record WHAT that limit is
//
// A replica is at its limit when it has a sustained backlog, or when its arrival
// rate has reached the service rate observed while it was backlogged. At that
// moment its resident token count is a measurement of the limit, and it is stored
// per workload bucket — model, accelerator, role, request shape — because it is a
// property of that bucket, not of the individual replica or of the current cycle.
//
// Keeping the measurement out of the per-cycle path is what makes the estimate
// usable downstream, for two reasons that both bit earlier versions of this code:
//
//   - aggregateByVariant takes the MEDIAN of per-replica capacities. A number that
//     varied per replica with that replica's own load was not commensurable across
//     siblings: an idle replica's figure blended with a backlogged one's and lifted
//     variant capacity enough to turn a scale-up into a scale-down. A bucket
//     ceiling is identical for every replica of the variant, so the median is a
//     no-op and cannot mix kinds.
//   - a value recomputed from this cycle's lambda moved every cycle, which is a
//     scaling oscillation waiting to happen. A stored ceiling changes only as the
//     running minimum is lowered by a new observation or relaxed by decay, both of
//     which are slow by construction.
//
// See docs/plans/engine/rate-anchored-k2.md.

// EnableRateAnchoredK2 selects the rate-anchored estimator. It is a build-time
// constant rather than a ConfigMap setting: the estimator is under evaluation
// against the occupancy-based one and is not something an operator should be
// switching in a running cluster. Flip it here to run the comparison, and see
// docs/plans/engine/rate-anchored-k2.md for the validation plan.
//
// With this false, SaturationAnalyzer.serviceRates stays nil and every path in
// this file returns immediately, leaving the occupancy-based estimator unchanged.
const EnableRateAnchoredK2 = false

const (
	// ServiceRateWindow is how long an observation stays authoritative for its
	// bucket. A replica only calibrates while it is at its limit, which may be
	// rare, so this is generous relative to the optimize interval.
	ServiceRateWindow = 30 * time.Minute

	// ServiceRateSmoothingWindow is the time constant over which mu tracks the
	// completion rates observed under backlog. While a replica is backlogged it is
	// never idle, so every such sample IS its service rate — there is no reason to
	// prefer the largest, and a running maximum could only ratchet upward. It has to
	// move down as readily as up: when prompts get longer within a bucket the true
	// mu falls, and an estimate that lags overstates capacity and under-scales.
	//
	// Five minutes is responsive within a load stage without chasing a single scrape.
	ServiceRateSmoothingWindow = 5 * time.Minute

	// CeilingRelaxPerWindow is the factor the learned token ceiling relaxes by over
	// one ServiceRateWindow with no fresh observation. The ceiling is a running
	// minimum, so it relaxes *upward*: a single pessimistic measurement — taken
	// while a node was degraded, say — must not cap the variant forever. Capacity
	// drifts back toward the memory bound in the absence of evidence.
	CeilingRelaxPerWindow = 1.25

	// MinServiceRateSamples is how many qualifying observations a bucket needs
	// before its service rate is usable. One observation cannot distinguish a real
	// limit from a single slow interval.
	MinServiceRateSamples = 2

	// SaturationEnterRatio is the fraction of the service rate at which arrivals
	// are treated as having reached it. Slightly below 1 so the limit is recognised
	// just before it is crossed, and so a lambda hovering at mu does not toggle the
	// detector between cycles.
	SaturationEnterRatio = 0.95

	// MinResidenceSeconds and MaxResidenceSeconds bound the residence estimate used
	// as the arrival-smoothing time constant, so a garbage latency reading cannot
	// turn the average into either a passthrough or a frozen value.
	MinResidenceSeconds = 1.0
	MaxResidenceSeconds = 300.0

	// ArrivalSmoothingResetFactor is how many time constants may pass before the
	// previous arrival average is discarded rather than blended.
	ArrivalSmoothingResetFactor = 3.0

	// BucketPruneThreshold is the map size at which adding a new bucket first tries
	// pruning stale ones. Small enough that the map stays bounded, large enough that
	// a normal fleet never pays for a sweep.
	BucketPruneThreshold = 32

	// MinRateAnchoredFraction floors the learned ceiling at a fraction of k1. A
	// replica that stalled completely while requests queued would otherwise teach
	// the bucket a near-zero capacity and demand an unbounded scale-up.
	MinRateAnchoredFraction = 0.05
)

// bucketStore holds what has been learned about a workload bucket: the service
// rate observed while a replica could not keep up (mu, requests/second), and the
// resident token count measured at that moment (the compute-bound ceiling).
//
// Both are per bucket rather than per replica. Replicas of a variant run the same
// model on the same hardware, so a limit measured on one applies to all — which is
// what makes the value safe to put through aggregateByVariant's median.
type bucketStore struct {
	mu      sync.Mutex
	entries map[string]*bucketEntry
}

type bucketEntry struct {
	rate         float64 // mu: running max of completion rate under backlog
	rateSamples  int
	rateSeen     time.Time
	ceiling      float64 // running min of resident tokens at the limit
	ceilingSeen  time.Time
	ceilingKnown bool
	work         float64 // EWMA of residence x tokens-per-request, in token-seconds
	workSeen     time.Time
	workKnown    bool
	workFrozen   float64 // the value every replica of the bucket reads this cycle
	workSum      float64 // this cycle's samples, averaged at the next freeze
	workTauSum   float64 // and their time constants, averaged with them
	workCount    int
}

func newBucketStore() *bucketStore {
	return &bucketStore{entries: make(map[string]*bucketEntry)}
}

// entry returns the bucket's entry, creating it when absent, and opportunistically
// prunes buckets nothing has touched for HistoryEvictionTimeout. Callers hold s.mu.
//
// Pruning on write rather than from a periodic caller is deliberate: nothing in the
// engine currently drives eviction for the analyzer's other stores either, so a
// store that depended on being swept would grow without bound the moment the
// estimator was switched on. Buckets are keyed by model, accelerator, role, GPU
// count and request shape, so the map is small and a sweep is cheap.
func (s *bucketStore) entry(key string, now time.Time) *bucketEntry {
	e, ok := s.entries[key]
	if !ok {
		if len(s.entries) >= BucketPruneThreshold {
			s.pruneLocked(HistoryEvictionTimeout, now)
		}
		e = &bucketEntry{}
		s.entries[key] = e
	}
	return e
}

// pruneLocked drops buckets with no observation of either kind within timeout.
// Callers hold s.mu.
func (s *bucketStore) pruneLocked(timeout time.Duration, now time.Time) int {
	removed := 0
	for k, e := range s.entries {
		last := e.rateSeen
		if e.ceilingSeen.After(last) {
			last = e.ceilingSeen
		}
		if e.workSeen.After(last) {
			last = e.workSeen
		}
		if now.Sub(last) > timeout {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// ObserveRate folds a completion rate into the bucket's service-rate estimate.
// Callers must only pass rates measured while the replica had a backlog: with no
// backlog, completions equal arrivals at any load and say nothing about the limit.
//
// Symmetric by construction. The ceiling is a running minimum, so it errs toward
// less capacity and more replicas; a running maximum here would err the opposite
// way, toward more capacity and fewer replicas, and only decay slowly back. Under
// backlog every sample is a valid reading of the service rate, so the mean of them
// is both the better estimate and the one that moves in either direction.
func (s *bucketStore) ObserveRate(key string, rate float64, now time.Time) {
	if rate <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	if e.rateSamples == 0 {
		e.rate, e.rateSamples, e.rateSeen = rate, 1, now
		return
	}
	e.rateSamples++
	e.rate = ewmaStep(e.rate, rate, now.Sub(e.rateSeen).Seconds(), ServiceRateSmoothingWindow.Seconds())
	e.rateSeen = now
}

// Rate returns the bucket's service-rate estimate, decayed by age, and false when
// it has too few observations or has gone stale.
func (s *bucketStore) Rate(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || e.rateSamples < MinServiceRateSamples {
		return 0, false
	}
	if now.Sub(e.rateSeen) > ServiceRateWindow {
		return 0, false
	}
	if e.rate <= 0 {
		return 0, false
	}
	return e.rate, true
}

// ObserveCeiling records the resident token count measured while a replica was at
// its limit, keeping the running minimum — the lowest occupancy at which a replica
// was seen unable to keep up is the conservative reading of the bucket's capacity.
func (s *bucketStore) ObserveCeiling(key string, tokens float64, now time.Time) {
	if tokens <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	if !e.ceilingKnown {
		e.ceiling, e.ceilingKnown, e.ceilingSeen = tokens, true, now
		return
	}
	// Compare against the relaxed value so an old pessimistic reading gives way to
	// a fresh higher one instead of capping the bucket indefinitely.
	if relaxed := relaxCeiling(e.ceiling, now.Sub(e.ceilingSeen)); tokens <= relaxed {
		e.ceiling = tokens
	} else {
		e.ceiling = relaxed
	}
	e.ceilingSeen = now
}

// Ceiling returns the bucket's learned token ceiling, relaxed by age, and false
// when nothing has been measured or the measurement has gone stale.
func (s *bucketStore) Ceiling(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || !e.ceilingKnown {
		return 0, false
	}
	if now.Sub(e.ceilingSeen) > ServiceRateWindow {
		return 0, false
	}
	c := relaxCeiling(e.ceiling, now.Sub(e.ceilingSeen))
	if c <= 0 {
		return 0, false
	}
	return c, true
}

// ObserveWork records this replica's work-per-request — residence time multiplied
// by tokens per request, in token-seconds — as one sample of the bucket's operating
// point. tau is the averaging time constant; the quantity changes on the residence
// timescale, so that is what it is smoothed over at the next freeze.
//
// Samples accumulate rather than folding in one at a time, because every replica of
// a bucket reports in the same cycle at the same timestamp. Folding each in turn
// would give the first replica of the loop the entire weight — the rest arrive with
// a zero time delta and change nothing — making the bucket's operating point depend
// on iteration order. A replica that had just started, with a short residence, could
// then pull the whole bucket's capacity down and drive a spurious scale-up.
//
// The value is deliberately not read back here: see FrozenWork.
func (s *bucketStore) ObserveWork(key string, work, tau float64, now time.Time) {
	if work <= 0 || tau <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.entry(key, now)
	e.workSum += work
	e.workTauSum += tau
	e.workCount++
}

// FrozenWork returns the work-per-request every replica of the bucket scales by
// this cycle, and false when none has been frozen yet.
func (s *bucketStore) FrozenWork(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok || e.workFrozen <= 0 {
		return 0, false
	}
	if now.Sub(e.workSeen) > ServiceRateWindow {
		return 0, false
	}
	return e.workFrozen, true
}

// FreezeWork averages the samples each bucket collected last cycle, folds that mean
// into the bucket's EWMA, and publishes it as the value this cycle's replicas will
// read. It must be called once at the top of a cycle.
//
// Publishing at the cycle boundary is what makes the number identical across
// siblings: aggregateByVariant takes the MEDIAN of per-replica capacities, so a
// value that moved as the loop progressed would blend incommensurable figures. It
// costs one cycle of lag, which is the price of that guarantee. Analyze is never
// concurrent, so a plain sweep is enough.
func (s *bucketStore) FreezeWork(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, e := range s.entries {
		if e.workCount > 0 {
			n := float64(e.workCount)
			sample, tau := e.workSum/n, e.workTauSum/n
			e.workSum, e.workTauSum, e.workCount = 0, 0, 0
			if e.workKnown {
				e.work = ewmaStep(e.work, sample, now.Sub(e.workSeen).Seconds(), tau)
			} else {
				e.work, e.workKnown = sample, true
			}
			e.workSeen = now
		}
		if e.workKnown {
			e.workFrozen = e.work
		}
	}
}

// EvictStale drops buckets with no observation of either kind within timeout,
// returning the number removed. Mirrors EvictStaleHistory so the stores age out
// together.
func (s *bucketStore) EvictStale(timeout time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(timeout, now)
}

// relaxCeiling grows an unrefreshed ceiling so capacity drifts back toward the
// memory bound when no fresh evidence of a limit arrives.
func relaxCeiling(ceiling float64, age time.Duration) float64 {
	if age <= 0 {
		return ceiling
	}
	return ceiling * math.Pow(CeilingRelaxPerWindow, age.Seconds()/ServiceRateWindow.Seconds())
}

// arrivalSmoother holds a per-replica exponentially-weighted arrival rate.
//
// A completion happens one residence time after the arrival that caused it, so a
// completion-derived mu and an instantaneous lambda are measured on different time
// bases: during a ramp, completions still reflect the lighter load of W seconds
// ago and the comparison reads as saturation on a replica that is coping.
// Averaging lambda over roughly W puts the two on the same footing.
type arrivalSmoother struct {
	mu      sync.Mutex
	entries map[string]*arrivalEntry
}

type arrivalEntry struct {
	rate     float64
	observed time.Time
}

func newArrivalSmoother() *arrivalSmoother {
	return &arrivalSmoother{entries: make(map[string]*arrivalEntry)}
}

// Smooth folds a new arrival-rate sample into the replica's EWMA and returns the
// smoothed value. tau is the averaging time constant — the residence estimate.
// The weight is derived from the actual gap between samples, so an irregular
// optimize interval or a missed cycle does not distort the average.
func (s *arrivalSmoother) Smooth(key string, rate, tau float64, now time.Time) float64 {
	if rate <= 0 || tau <= 0 {
		return rate
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		// Keyed per pod, so this map grows with pod churn; prune on insert for the
		// same reason bucketStore does.
		if len(s.entries) >= BucketPruneThreshold {
			for k, old := range s.entries {
				if now.Sub(old.observed) > HistoryEvictionTimeout {
					delete(s.entries, k)
				}
			}
		}
		s.entries[key] = &arrivalEntry{rate: rate, observed: now}
		return rate
	}
	if dt := now.Sub(e.observed).Seconds(); dt > 0 {
		e.rate = ewmaStep(e.rate, rate, dt, tau)
		e.observed = now
	}
	return e.rate
}

// ewmaStep folds sample into prev with a weight derived from the actual gap between
// samples, so an irregular optimize interval or a missed cycle does not distort the
// average. A gap longer than a few time constants discards prev outright: it carries
// no information about the present.
func ewmaStep(prev, sample, dt, tau float64) float64 {
	if dt <= 0 || tau <= 0 {
		return prev
	}
	if dt > tau*ArrivalSmoothingResetFactor {
		return sample
	}
	return prev + (1-math.Exp(-dt/tau))*(sample-prev)
}

// EvictStale drops replicas not seen within timeout, returning the number removed.
func (s *arrivalSmoother) EvictStale(timeout time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for k, e := range s.entries {
		if now.Sub(e.observed) > timeout {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// residenceSeconds estimates how long a request occupies the replica: time to the
// first token plus one inter-token latency per output token. Both inputs are
// already collected per replica. Returns 0 when they are unavailable, which leaves
// the arrival rate unsmoothed rather than smoothing it by a made-up constant.
func residenceSeconds(rm domain.ReplicaMetrics) float64 {
	if rm.AvgITL <= 0 || rm.AvgOutputTokens <= 0 {
		return 0
	}
	w := rm.AvgTTFT + rm.AvgOutputTokens*rm.AvgITL
	if w <= 0 || math.IsNaN(w) || math.IsInf(w, 0) {
		return 0
	}
	if w < MinResidenceSeconds {
		return MinResidenceSeconds
	}
	if w > MaxResidenceSeconds {
		return MaxResidenceSeconds
	}
	return w
}

// serviceRateKey identifies a workload bucket.
//
// Role is part of the key because a prefill replica and a decode replica of the
// same model on the same accelerator are different services: they do different
// work per request and complete at entirely different rates. Sharing a bucket
// would let one calibrate the other's limit.
//
// The input bucket is part of it too, unlike the k2 history key: the limit is a
// property of the request shape, and 1000-token prompts and 300-token prompts are
// different services on the same hardware. Keying only by output length would
// average them.
func serviceRateKey(modelID, accelerator, role string, gpuCount int, avgInput, avgOutput float64) string {
	return fmt.Sprintf("%s|%s|%s|%d|in:%s|out:%s",
		modelID, accelerator, canonicalRole(role), gpuCount,
		classifyOutputLength(avgInput), classifyOutputLength(avgOutput))
}

// rateAnchoredK2 returns the compute-bound capacity in KV tokens for this
// replica's bucket, the load-independent reference to store, and which signal
// produced the capacity.
//
// The capacity is the bucket's learned ceiling, scaled down by how far this cycle's
// operating point sits below the one the ceiling was measured at — see
// residenceScaledCapacity for why that scaling is the point of the whole design.
// Before anything has been learned, a replica that is over its limit right now
// still reports its own occupancy, so the first overload is not missed.
//
// The reference return is the unscaled ceiling. Capacity moves with contention and
// so must never be persisted: the capacity store feeds variants with no live
// replicas and cross-variant estimation, both of which need a number that means
// "what this replica can do", not "what it is doing now".
//
// Returns false when nothing has been learned and the replica is not currently
// over its limit, in which case the caller falls through to the occupancy-based
// chain.
func (a *SaturationAnalyzer) rateAnchoredK2(
	rm domain.ReplicaMetrics,
	modelID string,
	role string,
	gpuCount int,
	k1 int64,
	queueThreshold float64,
	now time.Time,
) (int64, int64, k2Source, bool) {
	if a.serviceRates == nil {
		return 0, 0, 0, false
	}

	key := serviceRateKey(modelID, rm.AcceleratorName, role, gpuCount, rm.AvgInputTokens, rm.AvgOutputTokens)

	// Detector: rates decide whether this replica is at its limit.
	backlogged := float64(rm.QueueLength) >= queueThreshold
	if backlogged && rm.RequestRate > 0 {
		a.serviceRates.ObserveRate(key, rm.RequestRate, now)
	}
	atLimit := backlogged || a.arrivalsReachedServiceRate(rm, key, now)

	// Measurement: under backlog, resident tokens are what the limit is worth.
	//
	// Only under backlog. Arrivals reaching the service rate is enough to say the
	// replica is at its limit — it is how the limit is caught before a queue forms —
	// but it is not a measurement of the limit: with no queue, low occupancy means
	// the replica is keeping up comfortably, not that its ceiling has fallen.
	// Recording it would ratchet the running minimum down on evidence of health.
	if occupancy := limitOccupancy(rm); backlogged && occupancy > 0 {
		a.serviceRates.ObserveCeiling(key, occupancy, now)
	}

	// Every cycle, at the limit or not, contributes to the bucket's work-per-request.
	// It is the operating point, not a measurement of the limit, so it is recorded
	// unconditionally — the whole point is to know how far below the limit we sit.
	if residence := residenceSeconds(rm); residence > 0 {
		if work := residence * tokensPerRequest(rm); work > 0 {
			a.serviceRates.ObserveWork(key, work, residence, now)
		}
	}

	ceiling, ok := a.serviceRates.Ceiling(key, now)
	if !ok {
		return 0, 0, 0, false
	}
	// The three labels say which regime produced the number: at the limit right now,
	// carrying a limit measured earlier, or holding a limit measured earlier scaled
	// to a lighter operating point. That is what the offline replay has to separate.
	src := k2SrcRateAnchored
	if atLimit {
		src = k2SrcRateBacklog
	}

	reference := clampCeiling(ceiling, k1)
	capacity := reference
	if scaled, ok := a.residenceScaledCapacity(key, ceiling, now); ok {
		capacity = clampCeiling(scaled, k1)
		src = k2SrcRateResidence
	}
	return capacity, reference, src, true
}

// residenceScaledCapacity expresses the bucket's capacity at this cycle's operating
// point, and reports false when it should be left at the measured ceiling.
//
// A ceiling alone cannot stop the collapse that validation round 1 measured. Demand
// is resident tokens, lambda x W x tokensPerRequest, so it falls when replicas are
// added: contention drops, residence W drops, and the queue term disappears
// outright. Supply held flat against a shrinking demand reads as abundant spare
// capacity, and the fleet sheds the replicas that had just fixed the problem.
//
// By Little's law a replica at its limit holds mu x W x tokensPerRequest tokens, so
// that product IS the capacity in the units the engine speaks, at whatever operating
// point W describes. Scaling supply by it makes demand/supply equal lambda/mu, which
// does not move when replicas are added — only lambda per replica does. At the
// moment of calibration the two agree exactly (lambda = mu there, so the product
// equals the occupancy that set the ceiling), so nothing jumps when this engages.
//
// The result is clamped at the ceiling and never above it: W is derived from TTFT,
// which includes time queued, so a backlogged replica reports an inflated W. Letting
// that raise capacity would relax the bound exactly when the replica is failing. One
// direction only — contention below the calibration point may lower capacity,
// queueing above it may not raise it.
func (a *SaturationAnalyzer) residenceScaledCapacity(key string, ceiling float64, now time.Time) (float64, bool) {
	muRate, ok := a.serviceRates.Rate(key, now)
	if !ok {
		return 0, false
	}
	work, ok := a.serviceRates.FrozenWork(key, now)
	if !ok {
		return 0, false
	}
	scaled := muRate * work
	if scaled <= 0 || math.IsNaN(scaled) || math.IsInf(scaled, 0) {
		return 0, false
	}
	if scaled >= ceiling {
		return 0, false
	}
	return scaled, true
}

// limitOccupancy is the resident token count to record as a measurement of the
// limit. It prefers the instantaneous KV reading over TokensInUse, which is derived
// from max_over_time(...[1m]) — the highest occupancy of the last minute, not the
// occupancy at the moment the replica was seen unable to keep up. Since the ceiling
// is a running minimum, a peak-derived input biases it high in the one direction
// that costs replicas. Falls back when the instantaneous reading is unavailable.
func limitOccupancy(rm domain.ReplicaMetrics) float64 {
	if rm.KvUsageInstant > 0 && rm.TotalKvCapacityTokens > 0 {
		if tokens := rm.KvUsageInstant * float64(rm.TotalKvCapacityTokens); tokens > 0 &&
			!math.IsNaN(tokens) && !math.IsInf(tokens, 0) {
			return tokens
		}
	}
	return float64(rm.TokensInUse)
}

// tokensPerRequest is the KV footprint of one request of this bucket's shape.
func tokensPerRequest(rm domain.ReplicaMetrics) float64 {
	t := rm.AvgInputTokens + rm.AvgOutputTokens
	if t <= 0 || math.IsNaN(t) || math.IsInf(t, 0) {
		return 0
	}
	return t
}

// arrivalsReachedServiceRate reports whether arrivals have caught up with the
// service rate measured while the replica was backlogged — the limit being reached
// before a queue has formed.
//
// lambda comes from the EPP dispatch rate where available. Without EPP, and only
// when there is no queue, completions stand in for arrivals: everything that
// arrives is served within the window, so the two are equal. That substitution is
// invalid under backlog, which is why the caller checks the queue first.
func (a *SaturationAnalyzer) arrivalsReachedServiceRate(rm domain.ReplicaMetrics, key string, now time.Time) bool {
	muRate, ok := a.serviceRates.Rate(key, now)
	if !ok {
		return false
	}

	smoothingKey := rm.PodName
	if smoothingKey == "" {
		smoothingKey = key
	}
	lambda := a.arrivals.Smooth(smoothingKey, rm.ArrivalRate, residenceSeconds(rm), now)
	if lambda <= 0 {
		lambda = rm.RequestRate
	}
	if lambda <= 0 || math.IsNaN(lambda) || math.IsInf(lambda, 0) {
		return false
	}
	return lambda >= muRate*SaturationEnterRatio
}

// clampCeiling keeps a learned ceiling within usable bounds. There is no upper
// clamp: min(k1, k2) in the caller already prevents a compute bound from exceeding
// the memory bound.
func clampCeiling(tokens float64, k1 int64) int64 {
	if math.IsNaN(tokens) || math.IsInf(tokens, 0) {
		return k1
	}
	if floor := float64(k1) * MinRateAnchoredFraction; tokens < floor {
		tokens = floor
	}
	if tokens <= 0 {
		return k1
	}
	return int64(tokens)
}
