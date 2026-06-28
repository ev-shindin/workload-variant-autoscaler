// Package stabilization implements HorizontalPodAutoscaler-style damping of
// raw replica recommendations.
//
// The workload-variant-autoscaler optimizer produces a fresh per-variant
// replica target every reconcile cycle. Acting on those targets directly is
// prone to flapping: a momentary load spike scales up and the following cycle
// scales straight back down. Kubernetes' HorizontalPodAutoscaler solves this
// with a configurable scaling behavior — a trailing stabilization window plus
// per-period rate policies. This package ports that behavior so WVA can damp
// its own recommendations before they are emitted.
//
// The configuration types are reused verbatim from k8s.io/api/autoscaling/v2
// (HorizontalPodAutoscalerBehavior, HPAScalingRules, HPAScalingPolicy) so an
// operator expresses stabilization the familiar HPA way. The algorithm is a
// clean-room implementation of the behavior documented at
// https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/#configurable-scaling-behavior
// it does not import k8s.io/kubernetes, which is not consumable as a module.
package stabilization

import (
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// DefaultDownscaleStabilizationWindow is the trailing window over which the
// largest recent recommendation is held to damp scale-down. It matches the
// upstream HorizontalPodAutoscaler default.
const DefaultDownscaleStabilizationWindow = 300 * time.Second

// maxStabilizationWindow bounds how long any single recommendation sample is
// retained, matching the upstream HPA ceiling on stabilizationWindowSeconds.
const maxStabilizationWindow = 3600 * time.Second

// Clock returns the current time. It is injectable so tests can drive the
// trailing windows deterministically.
type Clock func() time.Time

// recommendation is a single timestamped raw replica recommendation, retained
// to compute the trailing stabilization window.
type recommendation struct {
	timestamp time.Time
	replicas  int32
}

// scaleEvent records that an actuation changed the replica count by a positive
// magnitude at a point in time. Up and down events are tracked separately, as
// the rate policies budget each direction independently.
type scaleEvent struct {
	timestamp time.Time
	replicas  int32
}

// Stabilizer damps raw replica recommendations using HPA-style scaling
// behavior. It is safe for concurrent use and retains per-key history across
// calls, so a single long-lived instance should be shared across reconcile
// cycles (mirroring the HPA controller, which keeps this state in memory on the
// elected leader).
type Stabilizer struct {
	now Clock

	mu              sync.Mutex
	recommendations map[string][]recommendation
	upEvents        map[string][]scaleEvent
	downEvents      map[string][]scaleEvent
}

// Option customizes a Stabilizer.
type Option func(*Stabilizer)

// WithClock overrides the time source. Intended for tests.
func WithClock(c Clock) Option {
	return func(s *Stabilizer) {
		if c != nil {
			s.now = c
		}
	}
}

// New constructs a Stabilizer with empty history.
func New(opts ...Option) *Stabilizer {
	s := &Stabilizer{
		now:             time.Now,
		recommendations: make(map[string][]recommendation),
		upEvents:        make(map[string][]scaleEvent),
		downEvents:      make(map[string][]scaleEvent),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Args is the input to a single Stabilize call for one scale target.
type Args struct {
	// Key uniquely identifies the scale target across cycles (e.g.
	// "namespace/variant" or "namespace/variant/role"). History is retained
	// per Key.
	Key string
	// CurrentReplicas is the scale target's current replica count.
	CurrentReplicas int32
	// DesiredReplicas is the optimizer's raw recommendation for this cycle.
	DesiredReplicas int32
	// MinReplicas is the hard lower bound. The result is never below it.
	MinReplicas int32
	// MaxReplicas is the hard upper bound. Ignored when <= 0.
	MaxReplicas int32
	// Behavior carries the scale-up/scale-down rules. A nil Behavior, or a nil
	// direction within it, falls back to the HPA defaults.
	Behavior *autoscalingv2.HorizontalPodAutoscalerBehavior
}

// Result is the outcome of a Stabilize call.
type Result struct {
	// Replicas is the final stabilized, rate-limited and clamped target to act
	// on.
	Replicas int32
	// Stabilized is the recommendation after the trailing window and tolerance
	// deadband, before rate policies and min/max clamping. Exposed for
	// observability.
	Stabilized int32
	// Reason is a short human-readable explanation of why Replicas differs from
	// the raw DesiredReplicas, suitable for structured logs.
	Reason string
}

// Stabilize damps a single raw recommendation. It records the recommendation
// for future cycles and returns the target to act on. The pipeline is, in
// order: trailing stabilization window, tolerance deadband, per-period rate
// policies, then min/max clamp — matching the HorizontalPodAutoscaler.
func (s *Stabilizer) Stabilize(args Args) Result {
	now := s.now()
	up := resolveRules(behaviorScaleUp(args.Behavior), true)
	down := resolveRules(behaviorScaleDown(args.Behavior), false)

	// One critical section for the whole operation so concurrent calls — even on
	// the same key — observe and update the per-key history atomically.
	s.mu.Lock()
	defer s.mu.Unlock()

	stabilized := s.stabilizationWindowLocked(args.Key, args.CurrentReplicas, args.DesiredReplicas, now, up.window, down.window)
	stabilized = applyTolerance(args.CurrentReplicas, stabilized, up.tolerance, down.tolerance)

	rateLimited := s.ratePoliciesLocked(args.Key, args.CurrentReplicas, stabilized, now, up, down)

	final := clamp(rateLimited, args.MinReplicas, args.MaxReplicas)

	s.recordScaleEventLocked(args.Key, args.CurrentReplicas, final, now)

	return Result{
		Replicas:   final,
		Stabilized: stabilized,
		Reason:     reason(args.DesiredReplicas, stabilized, rateLimited, final),
	}
}

// Forget drops all retained history for keys not present in active. The engine
// calls it each cycle with the keys it is about to stabilize, bounding the
// per-key maps to the live set of scale targets (variants come and go).
func (s *Stabilizer) Forget(active map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.recommendations {
		if _, ok := active[k]; !ok {
			delete(s.recommendations, k)
			delete(s.upEvents, k)
			delete(s.downEvents, k)
		}
	}
}

// stabilizationWindowLocked records the raw recommendation and returns the
// current replica count clamped into the band [min recent (up window), max
// recent (down window)]. The smallest recommendation over the scale-up window
// is a floor (scale up only when every recent recommendation agrees); the
// largest recommendation over the scale-down window is a ceiling (scale down
// only after the window has decayed).
// stabilizationWindowLocked must be called with s.mu held.
func (s *Stabilizer) stabilizationWindowLocked(key string, current, desired int32, now time.Time, upWindow, downWindow time.Duration) int32 {
	upCutoff := now.Add(-upWindow)
	downCutoff := now.Add(-downWindow)
	pruneCutoff := now.Add(-max(upWindow, downWindow))

	upRec := desired   // floor for scale-up: min over the up window
	downRec := desired // ceiling for scale-down: max over the down window

	hist := s.recommendations[key]
	kept := hist[:0]
	for _, r := range hist {
		if r.timestamp.Before(pruneCutoff) {
			continue // sample older than both windows; drop it
		}
		kept = append(kept, r)
		if r.timestamp.After(upCutoff) && r.replicas < upRec {
			upRec = r.replicas
		}
		if r.timestamp.After(downCutoff) && r.replicas > downRec {
			downRec = r.replicas
		}
	}
	s.recommendations[key] = append(kept, recommendation{timestamp: now, replicas: desired})

	rec := current
	if rec < upRec {
		rec = upRec
	}
	if rec > downRec {
		rec = downRec
	}
	return rec
}

// applyTolerance suppresses a scale change whose magnitude is within the
// per-direction tolerance fraction of the current replica count. A tolerance of
// zero disables the deadband for that direction.
func applyTolerance(current, stabilized int32, upTolerance, downTolerance float64) int32 {
	if current <= 0 || stabilized == current {
		return stabilized
	}
	change := math.Abs(float64(stabilized-current)) / float64(current)
	if stabilized > current && upTolerance > 0 && change < upTolerance {
		return current
	}
	if stabilized < current && downTolerance > 0 && change < downTolerance {
		return current
	}
	return stabilized
}

// ratePoliciesLocked caps the per-period change permitted by the scaling
// policies, budgeting against the changes already actuated within each policy's
// period.
// ratePoliciesLocked must be called with s.mu held.
func (s *Stabilizer) ratePoliciesLocked(key string, current, desired int32, now time.Time, up, down resolvedRules) int32 {
	if desired == current {
		return desired
	}

	upEvents := s.upEvents[key]
	downEvents := s.downEvents[key]

	if desired > current {
		limit := calculateScaleUpLimit(current, upEvents, now, up)
		if limit < current {
			limit = current
		}
		if desired > limit {
			return limit
		}
		return desired
	}

	limit := calculateScaleDownLimit(current, downEvents, now, down)
	if limit > current {
		limit = current
	}
	if desired < limit {
		return limit
	}
	return desired
}

// calculateScaleUpLimit returns the highest replica count scaling up may reach
// this cycle under the configured policies.
func calculateScaleUpLimit(current int32, upEvents []scaleEvent, now time.Time, rules resolvedRules) int32 {
	if rules.selectPolicy == autoscalingv2.DisabledPolicySelect {
		return current
	}
	// With the default Max policy select, the most permissive (highest) proposal
	// wins; with Min select, the most conservative (lowest) wins.
	useMin := rules.selectPolicy == autoscalingv2.MinChangePolicySelect
	result := int32(math.MinInt32)
	if useMin {
		result = math.MaxInt32
	}
	for _, p := range rules.policies {
		addedInPeriod := replicasChangePerPeriod(p.PeriodSeconds, upEvents, now)
		periodStart := current - addedInPeriod
		var proposed int32
		switch p.Type {
		case autoscalingv2.PodsScalingPolicy:
			proposed = periodStart + p.Value
		case autoscalingv2.PercentScalingPolicy:
			proposed = int32(math.Ceil(float64(periodStart) * (1 + float64(p.Value)/100)))
		default:
			continue
		}
		if useMin {
			result = min(result, proposed)
		} else {
			result = max(result, proposed)
		}
	}
	return result
}

// calculateScaleDownLimit returns the lowest replica count scaling down may
// reach this cycle under the configured policies.
func calculateScaleDownLimit(current int32, downEvents []scaleEvent, now time.Time, rules resolvedRules) int32 {
	if rules.selectPolicy == autoscalingv2.DisabledPolicySelect {
		return current
	}
	// For scale-down the proposals are lower bounds: with the default Max policy
	// select the most permissive (lowest) wins; with Min select the most
	// conservative (highest) wins.
	useMax := rules.selectPolicy == autoscalingv2.MinChangePolicySelect
	result := int32(math.MaxInt32)
	if useMax {
		result = math.MinInt32
	}
	for _, p := range rules.policies {
		removedInPeriod := replicasChangePerPeriod(p.PeriodSeconds, downEvents, now)
		periodStart := current + removedInPeriod
		var proposed int32
		switch p.Type {
		case autoscalingv2.PodsScalingPolicy:
			proposed = periodStart - p.Value
		case autoscalingv2.PercentScalingPolicy:
			proposed = int32(float64(periodStart) * (1 - float64(p.Value)/100))
		default:
			continue
		}
		if proposed < 0 {
			proposed = 0
		}
		if useMax {
			result = max(result, proposed)
		} else {
			result = min(result, proposed)
		}
	}
	return result
}

// replicasChangePerPeriod sums the magnitude of events newer than periodSeconds
// ago, giving how many replicas were already added (or removed) within the
// current policy period.
func replicasChangePerPeriod(periodSeconds int32, events []scaleEvent, now time.Time) int32 {
	cutoff := now.Add(-time.Duration(periodSeconds) * time.Second)
	var sum int32
	for _, e := range events {
		if e.timestamp.After(cutoff) {
			sum += e.replicas
		}
	}
	return sum
}

// recordScaleEventLocked records an actuation's replica delta so future cycles
// can budget rate policies against it. Events older than the stabilization
// ceiling are pruned opportunistically. Must be called with s.mu held.
//
// The delta is recorded against the value this stage produced (final), so the
// rate budget stays accurate only while stabilization is the last stage that
// determines the actuated replica count. Any future post-stabilization clamp in
// the V2 path must update this event instead, or the budget will desync.
func (s *Stabilizer) recordScaleEventLocked(key string, from, to int32, now time.Time) {
	if to == from {
		return
	}
	pruneCutoff := now.Add(-maxStabilizationWindow)
	stale := func(e scaleEvent) bool { return !e.timestamp.After(pruneCutoff) }
	if to > from {
		s.upEvents[key] = append(slices.DeleteFunc(s.upEvents[key], stale), scaleEvent{timestamp: now, replicas: to - from})
	} else {
		s.downEvents[key] = append(slices.DeleteFunc(s.downEvents[key], stale), scaleEvent{timestamp: now, replicas: from - to})
	}
}

// clamp bounds v to [min, max]. The max bound is ignored when max <= 0.
func clamp(v, min, max int32) int32 {
	if max > 0 && v > max {
		v = max
	}
	if v < min {
		v = min
	}
	return v
}

func reason(desired, stabilized, rateLimited, final int32) string {
	switch {
	case final == desired:
		return "no stabilization applied"
	case final != rateLimited:
		return fmt.Sprintf("clamped to bounds: desired=%d final=%d", desired, final)
	case rateLimited != stabilized:
		return fmt.Sprintf("rate-limited: desired=%d stabilized=%d final=%d", desired, stabilized, final)
	default:
		return fmt.Sprintf("stabilization window applied: desired=%d final=%d", desired, final)
	}
}
