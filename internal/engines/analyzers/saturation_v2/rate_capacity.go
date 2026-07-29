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

	// MinRateRatio and MaxRateRatio clamp mu/lambda. A mis-scraped or transiently
	// zero rate must not be able to swing capacity by orders of magnitude.
	MinRateRatio = 0.05
	MaxRateRatio = 20.0

	// MinServiceRateSamples is how many qualifying observations a bucket needs
	// before its service rate is usable. One observation cannot distinguish a real
	// ceiling from a single slow interval.
	MinServiceRateSamples = 2

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

// serviceRateKey identifies a workload bucket for service-rate purposes.
//
// The input bucket is part of the key, unlike the k2 history key: mu is a property
// of the request shape, and 1000-token prompts and 300-token prompts are different
// services on the same hardware. Keying only by output length would average them.
func serviceRateKey(modelID, accelerator string, gpuCount int, avgInput, avgOutput float64) string {
	return fmt.Sprintf("%s|%s|%d|in:%s|out:%s",
		modelID, accelerator, gpuCount,
		classifyOutputLength(avgInput), classifyOutputLength(avgOutput))
}

// rateAnchoredK2 estimates the compute-bound capacity in KV tokens from the
// replica's distance to rate saturation, returning the value, which signal
// produced it, and whether that value is a measured ceiling.
//
// The ceiling flag governs persistence, not the scaling decision. At lambda >= mu
// the replica is at or past saturation, so occupancy × mu/lambda is what it was
// actually able to hold — worth writing to the capacity store. Below saturation
// the same expression is occupancy scaled by headroom: correct for this cycle's
// utilization, but not a property of the variant, and persisting it would
// under-state capacity for zero-replica estimation and cross-variant lookups.
//
// Returns false when no signal is usable, in which case the caller falls through
// to the existing occupancy-based chain.
func (a *SaturationAnalyzer) rateAnchoredK2(
	rm domain.ReplicaMetrics,
	modelID string,
	gpuCount int,
	k1 int64,
	queueThreshold float64,
	now time.Time,
) (int64, k2Source, bool, bool) {
	if a.serviceRates == nil {
		return 0, 0, false, false
	}

	key := serviceRateKey(modelID, rm.AcceleratorName, gpuCount, rm.AvgInputTokens, rm.AvgOutputTokens)

	// Only a sustained backlog establishes a ceiling; see the package comment.
	backlogged := float64(rm.QueueLength) >= queueThreshold
	if backlogged && rm.RequestRate > 0 {
		a.serviceRates.Observe(key, rm.RequestRate, now)
	}

	// KvUsageInstant is the un-peaked reading; the 1-minute max used for demand
	// would over-state the operating point and inflate the estimate.
	occupancy := rm.KvUsageInstant * float64(rm.TotalKvCapacityTokens)
	if occupancy <= 0 {
		return 0, 0, false, false
	}

	ratio, src, ok := a.saturationRatio(rm, key, backlogged, now)
	if !ok {
		return 0, 0, false, false
	}
	// At or past saturation the replica demonstrated this capacity; below it, the
	// value is headroom-scaled and must not be persisted as a ceiling.
	ceiling := ratio <= 1.0
	if ratio < MinRateRatio {
		ratio = MinRateRatio
	} else if ratio > MaxRateRatio {
		ratio = MaxRateRatio
	}

	k2 := occupancy * ratio
	if floor := float64(k1) * MinRateAnchoredFraction; k2 < floor {
		k2 = floor
	}
	if k2 <= 0 || math.IsNaN(k2) || math.IsInf(k2, 0) {
		return 0, 0, false, false
	}
	return int64(k2), src, ceiling, true
}

// saturationRatio returns mu/lambda — how much service headroom the replica has —
// from the best signal available, in descending order of directness:
//
//  1. EPP arrival rate. The intended path: lambda is measured independently of the
//     engine, so the ratio holds whether or not the replica is keeping up.
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
	backlogged bool,
	now time.Time,
) (float64, k2Source, bool) {
	muRate, haveMu := a.serviceRates.Rate(key, now)

	if rm.ArrivalRate > 0 && haveMu {
		return muRate / rm.ArrivalRate, k2SrcRateAnchored, true
	}
	if backlogged {
		return 1.0, k2SrcRateBacklog, true
	}
	if haveMu && rm.RequestRate > 0 {
		return muRate / rm.RequestRate, k2SrcRateNoEPP, true
	}
	return 0, 0, false
}
