package saturation

import (
	"context"

	"k8s.io/utils/ptr"
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

	// Bound the stabilizer's per-key history to the variants live this cycle, so
	// keys for deleted variants do not accumulate.
	active := make(map[string]struct{}, len(decisions))
	for i := range decisions {
		active[stabilizationKey(&decisions[i])] = struct{}{}
	}
	e.stabilizer.Retain(active)

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
			Behavior:        behavior,
			// 0 means no floor / no cap: scale-to-zero and minimum-replica
			// enforcement remain the enforcer's job, not the stabilizer's.
			MinReplicas: int32(ptr.Deref(d.MinReplicas, 0)),
			MaxReplicas: int32(ptr.Deref(d.MaxReplicas, 0)),
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
	category := d.ReasonCategory()
	if category == "" {
		category = interfaces.DecisionReasonV2
	}
	d.SetDecisionReason(d.ActionForTarget(), category, string(category)+" (stabilized)")
}
