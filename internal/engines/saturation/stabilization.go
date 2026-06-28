package saturation

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/stabilization"
)

// applyStabilization damps the optimizer's raw per-variant targets with
// HPA-style scaling behavior before the enforcer and emit path consume them.
// It mutates decisions in place and is a no-op unless EnableStabilization is set
// in the global ("default") saturation config — mirroring how the GPU limiter is
// gated. Each variant (per role, when disaggregated) is stabilized independently
// against its own trailing recommendation window and per-period rate budget,
// retained on the Engine's long-lived stabilizer.
func (e *Engine) applyStabilization(ctx context.Context, decisions []interfaces.VariantDecision) {
	if e.stabilizer == nil || len(decisions) == 0 {
		return
	}

	cfg, ok := e.Config.SaturationConfig()["default"]
	if !ok || !cfg.EnableStabilization {
		return
	}

	logger := ctrl.LoggerFrom(ctx)
	behavior := stabilization.DefaultBehavior()

	type stabilizationEntry struct {
		Name  string `json:"name"`
		Role  string `json:"role,omitempty"`
		Curr  int    `json:"curr"`
		Raw   int    `json:"raw"`   // optimizer recommendation
		Final int    `json:"final"` // after stabilization
	}
	type modelKey struct{ ns, modelID string }
	grouped := make(map[modelKey][]stabilizationEntry)

	for i := range decisions {
		d := &decisions[i]
		res := e.stabilizer.Stabilize(stabilization.Args{
			Key:             stabilizationKey(d),
			CurrentReplicas: int32(d.CurrentReplicas),
			DesiredReplicas: int32(d.TargetReplicas),
			MinReplicas:     intPtrOrZero(d.MinReplicas),
			MaxReplicas:     intPtrOrZero(d.MaxReplicas),
			Behavior:        behavior,
		})

		raw := d.TargetReplicas
		final := int(res.Replicas)
		if final != raw {
			d.TargetReplicas = final
			retargetDecision(d)
		}

		k := modelKey{d.Namespace, d.ModelID}
		grouped[k] = append(grouped[k], stabilizationEntry{
			Name:  d.VariantName,
			Role:  d.Role,
			Curr:  d.CurrentReplicas,
			Raw:   raw,
			Final: final,
		})
	}

	for k, entries := range grouped {
		logger.Info("stabilization-decision",
			"modelID", k.modelID,
			"namespace", k.ns,
			"decisions", entries,
		)
	}
}

// stabilizationKey identifies a scale target across cycles. Disaggregated P/D
// variants have one scale target per role, so the role is part of the key.
func stabilizationKey(d *interfaces.VariantDecision) string {
	key := d.Namespace + "/" + d.VariantName
	if d.Role != "" {
		key += "/" + d.Role
	}
	return key
}

// retargetDecision recomputes a decision's Action after stabilization changed
// its TargetReplicas, preserving the original reason category so the decision is
// not mis-attributed in the reason metric label.
func retargetDecision(d *interfaces.VariantDecision) {
	var action interfaces.SaturationAction
	switch {
	case d.TargetReplicas > d.CurrentReplicas:
		action = interfaces.ActionScaleUp
	case d.TargetReplicas < d.CurrentReplicas:
		action = interfaces.ActionScaleDown
	default:
		action = interfaces.ActionNoChange
	}
	category := d.ReasonCategory()
	if category == "" {
		category = interfaces.DecisionReasonV2
	}
	d.SetDecisionReason(action, category, string(category)+" (stabilized)")
}

// intPtrOrZero dereferences an optional replica bound, returning 0 when unset.
// A zero MaxReplicas means "no cap"; a zero MinReplicas means "no floor"
// (scale-to-zero and minimum-replica enforcement remain the enforcer's job).
func intPtrOrZero(v *int) int32 {
	if v == nil {
		return 0
	}
	return int32(*v)
}
