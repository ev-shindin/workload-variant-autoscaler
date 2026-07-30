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
// This estimator anchors on the one quantity that is meaningful whichever resource
// binds: while a replica cannot keep up, its completion rate IS its service rate.
//
// "Cannot keep up" has to be established, not assumed. With no backlog, completions
// equal arrivals at any load — flow conservation — so an idle replica's completion
// rate says nothing about its ceiling. mu is therefore recorded only from replicas
// with a queue at least QueueLengthThreshold deep, and a bucket needs
// MinServiceRateSamples such observations before it will answer. Without that,
// one burst of arrival jitter would pin mu at whatever the replica happened to be
// doing, and every later lambda near that value would read as saturation at low
// occupancy — over-provisioning, the mirror of the bug this replaces.
//
//	mu = highest completion rate seen for this workload bucket while queued
//	k2 = occupancy × (mu / lambda)
//
// At lambda == mu the replica is exactly saturated and k2 equals its current
// occupancy, so utilization reads 100% even at 16% KV. At lambda == mu/2, k2 is
// twice the occupancy and utilization reads 50%. The result stays in KV tokens, so
// every downstream consumer (min(k1,k2), spareCapacity, the optimizer) is unchanged.
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
	// ServiceRateWindow is how long an observed service rate stays authoritative
	// for its bucket. A replica only calibrates while it has a backlog, which may
	// be rare, so this is generous relative to the optimize interval.
	ServiceRateWindow = 30 * time.Minute

	// ServiceRateDecayPerWindow is the fraction the retained maximum decays to
	// over one ServiceRateWindow with no fresh observation. Decay matters because
	// mu is a running maximum: without it, one unusually fast interval would pin
	// the estimate high for as long as the entry lives.
	ServiceRateDecayPerWindow = 0.75

	// MinRateRatio floors mu/lambda so a mis-scraped or transiently collapsed
	// service rate cannot drive capacity to nearly zero. There is no upper clamp:
	// a ratio above 1 means the replica has headroom, and the estimator declines
	// rather than reporting a headroom-scaled figure as capacity.
	MinRateRatio = 0.05

	// MinServiceRateSamples is how many qualifying observations a bucket needs
	// before its service rate is usable. One observation cannot distinguish a real
	// ceiling from a single slow interval.
	MinServiceRateSamples = 2

	// MinResidenceSeconds and MaxResidenceSeconds bound the residence estimate used
	// as the arrival-smoothing time constant, so a garbage latency reading cannot
	// turn the average into either a passthrough or a frozen value.
	MinResidenceSeconds = 1.0
	MaxResidenceSeconds = 300.0

	// ArrivalSmoothingResetFactor is how many time constants may pass before the
	// previous arrival average is discarded rather than blended.
	ArrivalSmoothingResetFactor = 3.0

	// MinRateAnchoredFraction floors the estimate at a fraction of k1. A replica
	// that has stalled completely (mu near zero while requests are queued) would
	// otherwise drive capacity to ~0 and demand an unbounded scale-up.
	MinRateAnchoredFraction = 0.05
)

// serviceRateStore holds the observed service rate (mu, requests/second) per
// workload bucket. Entries are updated only from replicas that had a backlog at
// the time of observation — an idle replica's completion rate reflects arrivals,
// not capacity.
type serviceRateStore struct {
	mu      sync.Mutex
	entries map[string]*serviceRateEntry
}

type serviceRateEntry struct {
	rate     float64
	observed time.Time
	samples  int
}

func newServiceRateStore() *serviceRateStore {
	return &serviceRateStore{entries: make(map[string]*serviceRateEntry)}
}

// Observe records a completion rate for a bucket, keeping the running maximum.
// Callers must only pass rates measured while the replica had a backlog.
func (s *serviceRateStore) Observe(key string, rate float64, now time.Time) {
	if rate <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		s.entries[key] = &serviceRateEntry{rate: rate, observed: now, samples: 1}
		return
	}
	e.samples++
	// Compare against the decayed value so a stale peak yields to a fresh, lower
	// observation rather than persisting until eviction.
	if decayed := decayRate(e.rate, now.Sub(e.observed)); rate >= decayed {
		e.rate = rate
	} else {
		e.rate = decayed
	}
	e.observed = now
}

// Rate returns the current service-rate estimate for a bucket, decayed by age,
// and false when the bucket has no usable observation.
func (s *serviceRateStore) Rate(key string, now time.Time) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
		return 0, false
	}
	if e.samples < MinServiceRateSamples {
		return 0, false
	}
	if now.Sub(e.observed) > ServiceRateWindow {
		return 0, false
	}
	r := decayRate(e.rate, now.Sub(e.observed))
	if r <= 0 {
		return 0, false
	}
	return r, true
}

// EvictStale drops entries with no observation within timeout, returning the
// number removed. Mirrors EvictStaleHistory so both stores age out together.
func (s *serviceRateStore) EvictStale(timeout time.Duration, now time.Time) int {
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

// decayRate applies exponential decay so an unrefreshed maximum loses authority
// smoothly: after one ServiceRateWindow it retains ServiceRateDecayPerWindow of
// its value.
func decayRate(rate float64, age time.Duration) float64 {
	if age <= 0 {
		return rate
	}
	windows := age.Seconds() / ServiceRateWindow.Seconds()
	return rate * math.Pow(ServiceRateDecayPerWindow, windows)
}

// arrivalSmoother holds a per-replica exponentially-weighted arrival rate.
//
// A completion happens one residence time after the arrival that caused it, so a
// completion-derived mu and an instantaneous lambda are measured on different time
// bases: during a ramp, completions still reflect the lighter load of W seconds
// ago and mu/lambda reads as saturation on a replica that is coping. Occupancy has
// the same property — it is a stock that already integrates arrivals over W.
// Averaging lambda over roughly W puts all three on the same footing.
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
	if rate <= 0 {
		return rate
	}
	if tau <= 0 {
		return rate
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[key]
	if !ok {
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

// serviceRateKey identifies a workload bucket for service-rate purposes.
//
// Role is part of the key because a prefill replica and a decode replica of the
// same model on the same accelerator are different services: they do different
// work per request and complete at entirely different rates. Sharing a bucket
// would let one calibrate the other's ceiling.
//
// The input bucket is part of it too, unlike the k2 history key: mu is a property
// of the request shape, and 1000-token prompts and 300-token prompts are different
// services on the same hardware. Keying only by output length would average them.
func serviceRateKey(modelID, accelerator, role string, gpuCount int, avgInput, avgOutput float64) string {
	return fmt.Sprintf("%s|%s|%s|%d|in:%s|out:%s",
		modelID, accelerator, canonicalRole(role), gpuCount,
		classifyOutputLength(avgInput), classifyOutputLength(avgOutput))
}

// rateAnchoredK2 estimates the compute-bound capacity in KV tokens from the
// replica's distance to rate saturation, returning the value and which signal
// produced it.
//
// It answers ONLY when the replica is at or past saturation (lambda >= mu). Below
// saturation, occupancy × mu/lambda is occupancy scaled by headroom — a statement
// about slack, not a capacity. Returning it would be wrong in two places: it is not
// a ceiling, so it must not be persisted; and aggregateByVariant takes the MEDIAN
// of per-replica capacities, where an idle replica's headroom-scaled number sits
// next to a backlogged sibling's genuine ceiling as though the two were
// commensurable. With one replica queueing twelve deep and an idle sibling, that
// median lifted variant capacity enough to turn a scale-up into a scale-down —
// reintroducing the shed-to-one this estimator exists to prevent, by a new route.
//
// Declining above saturation loses nothing: detecting saturation the occupancy path
// misses is the whole purpose, and when a replica genuinely has headroom the
// occupancy path's ceiling is the better answer. Everything this function returns
// is therefore a measured ceiling, which is what makes it safe to persist.
//
// Returns false when no signal is usable or the replica has headroom, in which case
// the caller falls through to the existing occupancy-based chain.
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

	// Only a sustained backlog establishes a ceiling; see the package comment.
	backlogged := float64(rm.QueueLength) >= queueThreshold
	if backlogged && rm.RequestRate > 0 {
		a.serviceRates.Observe(key, rm.RequestRate, now)
	}

	// Put lambda on the same time base as the completion-derived mu and the
	// occupancy stock, both of which integrate over a residence time.
	smoothingKey := rm.PodName
	if smoothingKey == "" {
		smoothingKey = key
	}
	lambda := a.arrivals.Smooth(smoothingKey, rm.ArrivalRate, residenceSeconds(rm), now)

	// Scale by the same occupancy figure demand is built from, not the instantaneous
	// reading. Demand is TokensInUse (a 1-minute max) plus the queue charge, so
	// dividing it by a capacity derived from an instantaneous reading would inflate
	// utilization by whatever the peak-to-instant spread happens to be — a spiky
	// replica would look far more saturated than a steady one at the same load.
	// Using the same aggregation on both sides makes the occupancy term cancel:
	// with no queue, utilization reduces to lambda/mu, which is the whole intent.
	occupancy := float64(rm.TokensInUse)
	if occupancy <= 0 {
		return 0, 0, false
	}

	ratio, src, ok := a.saturationRatio(rm, key, lambda, backlogged, now)
	if !ok {
		return 0, 0, false
	}
	// Headroom is not a capacity: stay silent and let the occupancy path answer.
	if ratio > 1.0 {
		return 0, 0, false
	}
	if ratio < MinRateRatio {
		ratio = MinRateRatio
	}

	k2 := occupancy * ratio
	// A replica with no queue is, by observation, coping with what it currently
	// holds, so its capacity is at least that. Without this floor a lambda that
	// momentarily edges past a conservatively-measured mu would cut capacity below
	// the load the replica is visibly handling and demand a scale-up on evidence
	// that does not exist. It binds only when ratio < 1 with an empty queue; a
	// backlogged replica is genuinely over capacity and keeps the raw value.
	if !backlogged && k2 < occupancy {
		k2 = occupancy
	}
	if floor := float64(k1) * MinRateAnchoredFraction; k2 < floor {
		k2 = floor
	}
	if k2 <= 0 || math.IsNaN(k2) || math.IsInf(k2, 0) {
		return 0, 0, false
	}
	return int64(k2), src, true
}

// saturationRatio returns mu/lambda — how much service headroom the replica has —
// from the best signal available, in descending order of directness:
//
//  1. EPP arrival rate, smoothed over a residence time. The intended path: lambda
//     is measured independently of the engine, so the ratio holds whether or not
//     the replica is keeping up. Smoothing matters because completions lag their
//     arrivals — see arrivalSmoother.
//  2. Backlog. A replica queueing at least QueueLengthThreshold deep is saturated
//     by observation: arrivals exceed service, so the ratio is at most 1 and
//     capacity is the current occupancy. This needs neither lambda nor a calibrated
//     mu, which makes it the safety net for a fleet with no EPP and no prior
//     calibration — and it is exactly the prefill-heavy case the occupancy
//     estimator misreads as idle. A shallower queue does not qualify: arrival
//     jitter produces one every so often at any load.
//  3. Completion rate as lambda. With no queue, everything that arrives is served
//     within the scrape window, so completions are arrivals. Valid only in that
//     case, which is why it sits behind the backlog check rather than in front.
//
// Returning false leaves the decision to the occupancy-based chain.
func (a *SaturationAnalyzer) saturationRatio(
	rm domain.ReplicaMetrics,
	key string,
	lambda float64,
	backlogged bool,
	now time.Time,
) (float64, k2Source, bool) {
	muRate, haveMu := a.serviceRates.Rate(key, now)

	if lambda > 0 && haveMu {
		return muRate / lambda, k2SrcRateAnchored, true
	}
	if backlogged {
		return 1.0, k2SrcRateBacklog, true
	}
	if haveMu && rm.RequestRate > 0 {
		return muRate / rm.RequestRate, k2SrcRateNoEPP, true
	}
	return 0, 0, false
}
