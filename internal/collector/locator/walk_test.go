package locator

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeReader(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

func TestWalkOwnersUp_PodReplicaSetDeployment(t *testing.T) {
	ns := "default"
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: ns, UID: "uid-d"},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rs",
			Namespace: ns,
			UID:       "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d",
				Controller: ptr.To(true),
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs",
				Controller: ptr.To(true),
			}},
		},
	}
	c := newFakeReader(deploy, rs, pod).Build()

	chain, err := walkOwnersUp(context.Background(), c, pod, ns, defaultMaxDepth)
	if err != nil {
		t.Fatalf("walkOwnersUp: %v", err)
	}
	want := []chainNode{
		{Namespace: ns, APIVersion: "v1", Kind: "Pod", Name: "p"},
		{Namespace: ns, APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs"},
		{Namespace: ns, APIVersion: "apps/v1", Kind: "Deployment", Name: "d"},
	}
	if len(chain) != len(want) {
		t.Fatalf("chain length = %d, want %d (chain=%v)", len(chain), len(want), chain)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Errorf("chain[%d] = %v, want %v", i, chain[i], want[i])
		}
	}
}

func TestWalkOwnersUp_StopsAtMaxDepth(t *testing.T) {
	ns := "default"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "uid-rs",
				Controller: ptr.To(true),
			}},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rs", Namespace: ns, UID: "uid-rs",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "uid-d",
				Controller: ptr.To(true),
			}},
		},
	}
	c := newFakeReader(pod, rs).Build()

	chain, err := walkOwnersUp(context.Background(), c, pod, ns, 1)
	if err != nil {
		t.Fatalf("walkOwnersUp: %v", err)
	}
	// maxDepth=1 means start + 1 owner; further hops are not taken.
	if len(chain) != 2 {
		t.Errorf("len=%d, want 2", len(chain))
	}
}
