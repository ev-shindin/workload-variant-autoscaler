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

	// ServiceRateDecayPerWindow is the fraction the retained service rate decays to
	// over one ServiceRateWindow with no fresh observation. Decay matters because
	// the rate is a running maximum: without it, one unusually fast interval would
	// pin it high for as long as the entry lives.
	ServiceRateDecayPerWindow = 0.75

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
		if now.Sub(last) > timeout {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// ObserveRate records a completion rate for a bucket, keeping the running maximum.
// Callers must only pass rates measured while the replica had a backlog: with no
// backlog, completions equal arrivals at any load and say nothing about the limit.
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
	// Compare against the decayed value so a stale peak yields to a fresh, lower
	// observation rather than persisting until eviction.
	if decayed := decayRate(e.rate, now.Sub(e.rateSeen)); rate >= decayed {
		e.rate = rate
	} else {
		e.rate = decayed
	}
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
	r := decayRate(e.rate, now.Sub(e.rateSeen))
	if r <= 0 {
		return 0, false
	}
	return r, true
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

// EvictStale drops buckets with no observation of either kind within timeout,
// returning the number removed. Mirrors EvictStaleHistory so the stores age out
// together.
func (s *bucketStore) EvictStale(timeout time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked(timeout, now)
}

// decayRate applies exponential decay so an unrefreshed service rate loses
// authority smoothly: after one ServiceRateWindow it retains
// ServiceRateDecayPerWindow of its value.
func decayRate(rate float64, age time.Duration) float64 {
	if age <= 0 {
		return rate
	}
	return rate * math.Pow(ServiceRateDecayPerWindow, age.Seconds()/ServiceRateWindow.Seconds())
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
	dt := now.Sub(e.observed).Seconds()
	if dt <= 0 {
		return e.rate
	}
	// Older than a few time constants: the previous value carries no information.
	if dt > tau*ArrivalSmoothingResetFactor {
		e.rate, e.observed = rate, now
		return rate
	}
	alpha := 1 - math.Exp(-dt/tau)
	e.rate += alpha * (rate - e.rate)
	e.observed = now
	return e.rate
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
// replica's bucket, and which signal produced it.
//
// The value is the bucket's learned ceiling wherever one exists, so it is the same
// for every replica of the variant and does not move with this cycle's arrival
// rate. Before anything has been learned, a replica that is over its limit right
// now still reports its own occupancy, so the first overload is not missed.
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
) (int64, k2Source, bool) {
	if a.serviceRates == nil {
		return 0, 0, false
	}

	key := serviceRateKey(modelID, rm.AcceleratorName, role, gpuCount, rm.AvgInputTokens, rm.AvgOutputTokens)

	// Detector: rates decide whether this replica is at its limit.
	backlogged := float64(rm.QueueLength) >= queueThreshold
	if backlogged && rm.RequestRate > 0 {
		a.serviceRates.ObserveRate(key, rm.RequestRate, now)
	}
	atLimit := backlogged || a.arrivalsReachedServiceRate(rm, key, now)

	// Whether anything was known before this cycle, purely so the source label can
	// distinguish a fresh measurement from a carried-over one. Both are the same
	// number and take the same path; the distinction is for the cycle log.
	_, hadCeiling := a.serviceRates.Ceiling(key, now)

	// Measurement: at the limit, resident tokens are what the limit is worth.
	occupancy := float64(rm.TokensInUse)
	if atLimit && occupancy > 0 {
		a.serviceRates.ObserveCeiling(key, occupancy, now)
	}

	ceiling, ok := a.serviceRates.Ceiling(key, now)
	if !ok {
		return 0, 0, false
	}
	src := k2SrcRateAnchored
	if !hadCeiling {
		src = k2SrcRateBacklog
	}
	return clampCeiling(ceiling, k1), src, true
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
