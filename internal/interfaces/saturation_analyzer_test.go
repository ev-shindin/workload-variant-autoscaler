package interfaces

import (
	"testing"
)

func TestVariantDecision_SetDecisionReason(t *testing.T) {
	tests := []struct {
		name           string
		action         SaturationAction
		reasonType     DecisionReason
		detailedReason string
		wantReasonType string
	}{
		{
			name:           "V2 scale-up",
			action:         ActionScaleUp,
			reasonType:     DecisionReasonV2,
			detailedReason: "V2 (optimizer: cost-aware)",
			wantReasonType: "V2",
		},
		{
			name:           "V2 scale-down",
			action:         ActionScaleDown,
			reasonType:     DecisionReasonV2,
			detailedReason: "V2 (optimizer: cost-aware)",
			wantReasonType: "V2",
		},
		{
			name:           "V2 steady state",
			action:         ActionNoChange,
			reasonType:     DecisionReasonV2,
			detailedReason: "V2",
			wantReasonType: "V2",
		},
		{
			name:           "V2 enforced",
			action:         ActionScaleUp,
			reasonType:     DecisionReasonV2,
			detailedReason: "V2 (optimizer: cost-aware, enforced)",
			wantReasonType: "V2",
		},
		{
			name:           "KV cache utilization above threshold",
			action:         ActionScaleUp,
			reasonType:     DecisionReasonKVCacheUtilAboveThreshold,
			detailedReason: "KV cache utilization above threshold",
			wantReasonType: "KV cache utilization above threshold",
		},
		{
			name:           "saturation-only mode",
			action:         ActionScaleUp,
			reasonType:     DecisionReasonSaturationOnly,
			detailedReason: "saturation-only mode: scale-up",
			wantReasonType: "saturation-only mode",
		},
		{
			name:           "scale-from-zero",
			action:         ActionScaleUp,
			reasonType:     DecisionReasonScaleFromZero,
			detailedReason: "scale-from-zero: pending request - scale-up",
			wantReasonType: "scale-from-zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := &VariantDecision{}
			decision.SetDecisionReason(tt.action, tt.reasonType, tt.detailedReason)

			if string(decision.DecisionReason()) != tt.wantReasonType {
				t.Errorf("SetDecisionReason() ReasonType = %v, want %v", decision.DecisionReason(), tt.wantReasonType)
			}
			if decision.Reason() != tt.detailedReason {
				t.Errorf("SetDecisionReason() Reason = %v, want %v", decision.Reason(), tt.detailedReason)
			}
		})
	}
}

func TestVariantDecision_SetDecisionReason_MultipleUpdates(t *testing.T) {
	decision := &VariantDecision{}

	// First update
	decision.SetDecisionReason(ActionScaleUp, DecisionReasonV2, "V2 (optimizer: cost-aware)")
	if string(decision.DecisionReason()) != "V2" {
		t.Errorf("First update: ReasonType = %v, want %v", decision.DecisionReason(), "V2")
	}
	if decision.Reason() != "V2 (optimizer: cost-aware)" {
		t.Errorf("First update: Reason = %v, want %v", decision.Reason(), "V2 (optimizer: cost-aware)")
	}

	// Second update should overwrite
	decision.SetDecisionReason(ActionScaleUp, DecisionReasonV2, "V2 (optimizer: cost-aware, enforced)")
	if string(decision.DecisionReason()) != "V2" {
		t.Errorf("Second update: ReasonType = %v, want %v", decision.DecisionReason(), "V2")
	}
	if decision.Reason() != "V2 (optimizer: cost-aware, enforced)" {
		t.Errorf("Second update: Reason = %v, want %v", decision.Reason(), "V2 (optimizer: cost-aware, enforced)")
	}
}
