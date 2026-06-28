package stabilization

import (
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
)

// resolvedRules is the normalized form of an HPAScalingRules for one direction,
// with all defaults filled in.
type resolvedRules struct {
	window       time.Duration
	selectPolicy autoscalingv2.ScalingPolicySelect
	policies     []autoscalingv2.HPAScalingPolicy
	tolerance    float64 // fraction of current replicas; 0 disables the deadband
}

// behaviorScaleUp returns the scale-up rules from a behavior, or nil if unset.
func behaviorScaleUp(b *autoscalingv2.HorizontalPodAutoscalerBehavior) *autoscalingv2.HPAScalingRules {
	if b == nil {
		return nil
	}
	return b.ScaleUp
}

// behaviorScaleDown returns the scale-down rules from a behavior, or nil if unset.
func behaviorScaleDown(b *autoscalingv2.HorizontalPodAutoscalerBehavior) *autoscalingv2.HPAScalingRules {
	if b == nil {
		return nil
	}
	return b.ScaleDown
}

// resolveRules fills in default values for any unset field of an HPAScalingRules.
// A nil rules value resolves entirely to the direction's defaults.
//
// Scale-up defaults: no stabilization window; allow the higher of +4 pods or
// +100% per 60s. Scale-down defaults: a 300s stabilization window; allow -100%
// per 60s, and a Max select policy.
//
// The pod/percent magnitudes (4, 100%) and the 300s scale-down window match the
// HorizontalPodAutoscaler. The policy period is 60s rather than the HPA
// controller's 15s default because WVA's optimize loop runs every ~30s, so a 60s
// window spans a couple of cycles whereas 15s would make the per-period budget a
// near no-op. (The autoscaling/v2 API documentation itself states the default as
// "per 60 seconds", while the controller's defaulting uses 15s.)
func resolveRules(rules *autoscalingv2.HPAScalingRules, scaleUp bool) resolvedRules {
	out := resolvedRules{selectPolicy: autoscalingv2.MaxChangePolicySelect}
	if scaleUp {
		out.policies = defaultScaleUpPolicies()
	} else {
		out.window = DefaultDownscaleStabilizationWindow
		out.policies = defaultScaleDownPolicies()
	}

	if rules == nil {
		return out
	}

	if rules.StabilizationWindowSeconds != nil {
		out.window = time.Duration(*rules.StabilizationWindowSeconds) * time.Second
	}
	if rules.SelectPolicy != nil {
		out.selectPolicy = *rules.SelectPolicy
	}
	if len(rules.Policies) > 0 {
		out.policies = rules.Policies
	}
	if rules.Tolerance != nil {
		out.tolerance = rules.Tolerance.AsApproximateFloat64()
	}
	return out
}

func defaultScaleUpPolicies() []autoscalingv2.HPAScalingPolicy {
	return []autoscalingv2.HPAScalingPolicy{
		{Type: autoscalingv2.PodsScalingPolicy, Value: 4, PeriodSeconds: 60},
		{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 60},
	}
}

func defaultScaleDownPolicies() []autoscalingv2.HPAScalingPolicy {
	return []autoscalingv2.HPAScalingPolicy{
		{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 60},
	}
}

// DefaultBehavior returns the HorizontalPodAutoscaler default scaling behavior:
// immediate scale-up rate-limited to the higher of +4 pods or +100% per 60s,
// and scale-down damped by a 300s stabilization window. Callers may use it as a
// starting point and override individual fields.
func DefaultBehavior() *autoscalingv2.HorizontalPodAutoscalerBehavior {
	// Each direction gets its own pointers so a caller overriding one field via
	// dereference (e.g. *b.ScaleUp.SelectPolicy = ...) cannot corrupt the other.
	upSelect := autoscalingv2.MaxChangePolicySelect
	downSelect := autoscalingv2.MaxChangePolicySelect
	upWindow := int32(0)
	downWindow := int32(DefaultDownscaleStabilizationWindow / time.Second)
	return &autoscalingv2.HorizontalPodAutoscalerBehavior{
		ScaleUp: &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: &upWindow,
			SelectPolicy:               &upSelect,
			Policies:                   defaultScaleUpPolicies(),
		},
		ScaleDown: &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: &downWindow,
			SelectPolicy:               &downSelect,
			Policies:                   defaultScaleDownPolicies(),
		},
	}
}
