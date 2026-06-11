/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0
*/

package indexers

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// scaleTargetIndexKey returns the composite index key for a scale target reference.
// Format: Namespace/APIVersion/Kind/Name (e.g., "default/apps/v1/Deployment/my-app").
// Shared by all per-resource index files (variantautoscaling.go, hpa.go, scaledobject.go).
func scaleTargetIndexKey(namespace string, ref autoscalingv2.CrossVersionObjectReference) string {
	if ref.APIVersion == "" {
		switch ref.Kind {
		case constants.DeploymentKind:
			ref.APIVersion = constants.DeploymentAPIVersion
		case constants.LeaderWorkerSetKind:
			ref.APIVersion = constants.LeaderWorkerSetAPIVersion
		default:
			logger := ctrl.LoggerFrom(context.TODO())
			logger.V(logging.DEBUG).Info("APIVersion not specified for scale target; defaulting to apps/v1", "kind", ref.Kind, "name", ref.Name)
			ref.APIVersion = constants.DeploymentAPIVersion
		}
	}
	return fmt.Sprintf("%s/%s/%s/%s", namespace, ref.APIVersion, ref.Kind, ref.Name)
}

// SetupIndexes registers custom field indexes with the manager's cache.
// Currently only the VariantAutoscaling index is registered here; HPA and
// ScaledObject indexes are added in Tasks 7 and 8.
func SetupIndexes(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, VAScaleTargetKey, VAScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for VariantAutoscaling: %w", err)
	}
	return nil
}
