package saturation

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// newRescaleTestEngine builds a minimal engine with a fake recorder and a fresh
// per-cycle event tracker, enough to exercise setRescaledCondition.
func newRescaleTestEngine() (*Engine, *record.FakeRecorder) {
	rec := record.NewFakeRecorder(10)
	return &Engine{Recorder: rec, vaEventTracker: map[string]bool{}}, rec
}

func newRescaleTestVA() *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	return &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "va-a", Namespace: "default"},
	}
}

// TestSetRescaledCondition_Reclaim sets the Rescaled condition with the Reclaim
// reason and a message carrying priority/share/current/scope, and emits no event.
func TestSetRescaledCondition_Reclaim(t *testing.T) {
	e, rec := newRescaleTestEngine()
	va := newRescaleTestVA()

	e.setRescaledCondition(context.Background(), va, &domain.RescaleDecisionInfo{
		Priority: 1, TargetGPUs: 2, CurrentGPUs: 8, Scope: "cluster",
		Action: domain.RescaleActionReclaim,
	})

	cond := llmdVariantAutoscalingV1alpha1.GetCondition(va, llmdVariantAutoscalingV1alpha1.TypeRescaled)
	if assert.NotNil(t, cond, "Rescaled condition must be set") {
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, llmdVariantAutoscalingV1alpha1.ReasonRescaleReclaim, cond.Reason)
		assert.Contains(t, cond.Message, "share=2GPU")
		assert.Contains(t, cond.Message, "current=8GPU")
		assert.Contains(t, cond.Message, "scope=cluster")
	}

	select {
	case ev := <-rec.Events:
		t.Errorf("no event expected for a clean reclaim, got %q", ev)
	default:
	}
}

// TestSetRescaledCondition_Fill uses the Fill reason for an under-share model.
func TestSetRescaledCondition_Fill(t *testing.T) {
	e, _ := newRescaleTestEngine()
	va := newRescaleTestVA()

	e.setRescaledCondition(context.Background(), va, &domain.RescaleDecisionInfo{
		Priority: 3, TargetGPUs: 6, CurrentGPUs: 0, Scope: "team",
		Action: domain.RescaleActionFill,
	})

	cond := llmdVariantAutoscalingV1alpha1.GetCondition(va, llmdVariantAutoscalingV1alpha1.TypeRescaled)
	if assert.NotNil(t, cond) {
		assert.Equal(t, llmdVariantAutoscalingV1alpha1.ReasonRescaleFill, cond.Reason)
		assert.Contains(t, cond.Message, "scope=team")
	}
}

// TestSetRescaledCondition_Stalled uses the Stalled reason (overriding the direction)
// and emits a RescaleStalled Warning event.
func TestSetRescaledCondition_Stalled(t *testing.T) {
	e, rec := newRescaleTestEngine()
	va := newRescaleTestVA()

	e.setRescaledCondition(context.Background(), va, &domain.RescaleDecisionInfo{
		Priority: 1, TargetGPUs: 0, CurrentGPUs: 1, Scope: "cluster",
		Action: domain.RescaleActionReclaim, Stalled: true,
	})

	cond := llmdVariantAutoscalingV1alpha1.GetCondition(va, llmdVariantAutoscalingV1alpha1.TypeRescaled)
	if assert.NotNil(t, cond) {
		assert.Equal(t, llmdVariantAutoscalingV1alpha1.ReasonRescaleStalled, cond.Reason,
			"Stalled reason overrides the reclaim direction")
	}

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, constants.K8SEventRescaleStalled)
		assert.True(t, strings.Contains(ev, "Warning"), "stall event must be a Warning: %q", ev)
	default:
		t.Error("expected a RescaleStalled Warning event, got none")
	}
}
