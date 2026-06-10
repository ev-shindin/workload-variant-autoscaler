// Package podvamap derives, once per optimization cycle, a mapping from pod
// identity to the VariantAutoscaling whose scale target owns the pod.
//
// It replaces the tenant-authored llm-d.ai/variant pod-template label as the
// default attribution path: for each managed VariantAutoscaling, WVA lists the
// pods behind its scale target's selector and attributes them to that VA. Pods
// matched by more than one workload selector are disambiguated by walking the
// owner chain (Pod -> ReplicaSet/StatefulSet -> Deployment/LeaderWorkerSet ->
// VA); if still ambiguous they are left unmapped and surface as a mapping miss.
//
// The map is read-only on the metrics hot path: callers resolve a pod with a
// single Lookup, with no Kubernetes calls.
package podvamap

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Map holds a per-cycle "<namespace>/<pod-name>" -> VariantAutoscaling-name mapping.
// A nil *Map is safe to Lookup against (it resolves nothing).
type Map struct {
	byPod map[string]string
}

// Lookup returns the VariantAutoscaling name a pod was attributed to, and whether
// the pod was resolved. Pods are keyed by namespace and name (pod names are not
// unique across namespaces).
func (m *Map) Lookup(namespace, podName string) (string, bool) {
	if m == nil || m.byPod == nil {
		return "", false
	}
	va, ok := m.byPod[namespace+"/"+podName]
	return va, ok
}

// Len returns the number of resolved pods. Used mainly in tests.
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.byPod)
}

// Build derives the pod -> VariantAutoscaling map for a set of managed VAs.
//
//   - reader is an uncached read interface (mgr.GetAPIReader()): a direct, bounded
//     API LIST/GET per cycle that does NOT start a cluster-wide Pod informer/cache.
//   - scaleTargets is keyed by "<namespace>/<scaleTargetName>" and provides the
//     workload (Deployment/LWS) backing each VA's scaleTargetRef.
//   - variantAutoscalings is keyed by "<namespace>/<vaName>".
//
// For each VA, the pods behind its scale target's selector are listed and attributed
// to that VA. Overlapping-selector collisions are resolved by owner reference;
// unresolved collisions increment the pod-mapping miss counter and are omitted.
func Build(
	ctx context.Context,
	reader client.Reader,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) *Map {
	logger := ctrl.LoggerFrom(ctx)

	// candidates: "<namespace>/<podName>" -> set of VA names whose selector matched it.
	candidates := make(map[string]map[string]struct{})
	// podObjs: same key -> Pod, retained only for collisions needing owner-ref tiebreak.
	podObjs := make(map[string]*corev1.Pod)

	// TODO: this issues one selector-scoped List per VA (O(VAs) API calls per cycle).
	// Acceptable at realistic VA counts; if it grows, switch to one List per namespace
	// plus in-memory selector matching.
	for vaKey, va := range variantAutoscalings {
		if va == nil {
			continue
		}
		accessor, ok := scaleTargets[va.Namespace+"/"+va.GetScaleTargetName()]
		if !ok || accessor == nil {
			logger.V(logging.DEBUG).Info("pod-VA derivation: no scale target for VA", "va", vaKey)
			continue
		}

		podList := &corev1.PodList{}
		if err := reader.List(ctx, podList,
			client.InNamespace(va.Namespace),
			client.MatchingLabelsSelector{Selector: accessor.GetPodSelector()},
		); err != nil {
			logger.V(logging.DEBUG).Info("pod-VA derivation: failed to list pods", "va", vaKey, "error", err)
			continue
		}

		for i := range podList.Items {
			pod := &podList.Items[i]
			key := pod.Namespace + "/" + pod.Name
			if candidates[key] == nil {
				candidates[key] = make(map[string]struct{})
				podObjs[key] = pod
			}
			candidates[key][va.Name] = struct{}{}
		}
	}

	byPod := make(map[string]string, len(candidates))
	for key, vaSet := range candidates {
		if len(vaSet) == 1 {
			for vaName := range vaSet {
				byPod[key] = vaName
			}
			continue
		}

		// Overlapping selectors: disambiguate via the owner chain.
		pod := podObjs[key]
		if vaName := resolveByOwnerRef(ctx, reader, pod, variantAutoscalings); vaName != "" {
			if _, matched := vaSet[vaName]; matched {
				byPod[key] = vaName
				continue
			}
		}
		// Still ambiguous: leave unmapped and record a miss. variant_name is empty
		// because the pod could not be attributed to a single VA — that is the signal.
		ns, name := "", key
		if pod != nil {
			ns, name = pod.Namespace, pod.Name
		}
		metrics.IncPodMappingMiss("", ns, constants.PodMappingMissAmbiguous)
		logger.V(logging.DEBUG).Info("pod-VA derivation: pod matched multiple workloads, owner-ref tiebreak failed",
			"pod", name, "namespace", ns)
	}

	return &Map{byPod: byPod}
}

// resolveByOwnerRef walks Pod -> ReplicaSet/StatefulSet -> Deployment/LeaderWorkerSet
// and resolves the owning VariantAutoscaling by matching the top-level workload against
// the in-memory variantAutoscalings (by scaleTargetRef), so it needs no field-index cache.
// It returns the VA name, or "" if the chain cannot be resolved. Used only to break
// overlapping-selector ties, so its per-pod reads are off the common path.
func resolveByOwnerRef(
	ctx context.Context,
	reader client.Reader,
	pod *corev1.Pod,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) string {
	if pod == nil {
		return ""
	}
	logger := ctrl.LoggerFrom(ctx)
	ns := pod.Namespace

	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return ""
	}

	var controllee metav1.Object
	switch owner.Kind {
	case constants.ReplicaSetKind:
		rs := &appsv1.ReplicaSet{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: owner.Name}, rs); err != nil {
			logger.V(logging.DEBUG).Info("pod-VA derivation: failed to get ReplicaSet", "replicaset", owner.Name, "error", err)
			return ""
		}
		controllee = rs
	case constants.StatefulSetKind:
		ss := &appsv1.StatefulSet{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: owner.Name}, ss); err != nil {
			logger.V(logging.DEBUG).Info("pod-VA derivation: failed to get StatefulSet", "statefulset", owner.Name, "error", err)
			return ""
		}
		controllee = ss
	default:
		return ""
	}

	top := metav1.GetControllerOf(controllee)
	if top == nil {
		return ""
	}

	// Match the top-level workload against the managed VAs already in memory.
	for _, va := range variantAutoscalings {
		if va != nil && va.Namespace == ns &&
			va.GetScaleTargetKind() == top.Kind && va.GetScaleTargetName() == top.Name {
			return va.Name
		}
	}
	return ""
}
