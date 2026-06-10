package podvamap

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

const testNS = "ns"

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add llmd v1alpha1: %v", err)
	}
	return scheme
}

func deployment(name string, selector map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
}

func va(name, scaleTargetName string) *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	return &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: scaleTargetName},
			ModelID:        "model",
		},
	}
}

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: labels}}
}

func TestBuild(t *testing.T) {
	ctx := context.Background()
	scheme := newScheme(t)

	t.Run("resolves pods that match the scale-target selector", func(t *testing.T) {
		deploy := deployment("deploy-a", map[string]string{"app": "a"})
		p1 := pod("pod-a-1", map[string]string{"app": "a"})
		p2 := pod("pod-a-2", map[string]string{"app": "a"})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, p1, p2).Build()

		scaleTargets := map[string]scaletarget.ScaleTargetAccessor{
			"ns/deploy-a": scaletarget.NewDeploymentAccessor(deploy),
		}
		vas := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			"ns/va-a": va("va-a", "deploy-a"),
		}

		m := Build(ctx, c, scaleTargets, vas)
		for _, name := range []string{"pod-a-1", "pod-a-2"} {
			got, ok := m.Lookup(testNS, name)
			if !ok || got != "va-a" {
				t.Errorf("Lookup(%q) = (%q, %v), want (va-a, true)", name, got, ok)
			}
		}
		if m.Len() != 2 {
			t.Errorf("Len() = %d, want 2", m.Len())
		}
	})

	t.Run("pods that match no selector are not resolved", func(t *testing.T) {
		deploy := deployment("deploy-a", map[string]string{"app": "a"})
		stray := pod("stray", map[string]string{"app": "other"})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, stray).Build()

		m := Build(ctx, c,
			map[string]scaletarget.ScaleTargetAccessor{"ns/deploy-a": scaletarget.NewDeploymentAccessor(deploy)},
			map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/va-a": va("va-a", "deploy-a")},
		)
		if _, ok := m.Lookup(testNS, "stray"); ok {
			t.Errorf("Lookup(stray) resolved unexpectedly")
		}
	})

	t.Run("VA without a known scale target is skipped", func(t *testing.T) {
		p1 := pod("pod-a-1", map[string]string{"app": "a"})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p1).Build()

		// scaleTargets is empty: the VA's scale target is not present.
		m := Build(ctx, c,
			map[string]scaletarget.ScaleTargetAccessor{},
			map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{"ns/va-a": va("va-a", "deploy-a")},
		)
		if m.Len() != 0 {
			t.Errorf("Len() = %d, want 0 (no scale target → nothing resolved)", m.Len())
		}
	})

	t.Run("overlapping selectors with no owner chain are left unresolved", func(t *testing.T) {
		deployA := deployment("deploy-a", map[string]string{"app": "shared"})
		deployB := deployment("deploy-b", map[string]string{"app": "shared"})
		shared := pod("shared-pod", map[string]string{"app": "shared"})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployA, deployB, shared).Build()

		scaleTargets := map[string]scaletarget.ScaleTargetAccessor{
			"ns/deploy-a": scaletarget.NewDeploymentAccessor(deployA),
			"ns/deploy-b": scaletarget.NewDeploymentAccessor(deployB),
		}
		vas := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			"ns/va-a": va("va-a", "deploy-a"),
			"ns/va-b": va("va-b", "deploy-b"),
		}

		// The pod matches both selectors and has no owner reference, so the owner-ref
		// tiebreak fails and the pod is left unresolved (a mapping miss) rather than
		// being misattributed to an arbitrary VA.
		m := Build(ctx, c, scaleTargets, vas)
		if _, ok := m.Lookup(testNS, "shared-pod"); ok {
			t.Errorf("Lookup(shared-pod) resolved despite ambiguous selectors")
		}
	})

	t.Run("nil map Lookup is safe", func(t *testing.T) {
		var m *Map
		if _, ok := m.Lookup(testNS, "anything"); ok {
			t.Errorf("nil Map.Lookup resolved unexpectedly")
		}
	})
}
