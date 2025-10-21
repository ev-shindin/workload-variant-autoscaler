package actuator

import (
	"context"
	"fmt"

	llmdOptv1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Actuator struct {
	Client         client.Client
	MetricsEmitter *metrics.MetricsEmitter
}

func NewActuator(k8sClient client.Client) *Actuator {
	return &Actuator{
		Client:         k8sClient,
		MetricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// getCurrentDeploymentReplicas gets the real current replica count from the actual Deployment
func (a *Actuator) getCurrentDeploymentReplicas(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling) (int32, error) {
	var deploy appsv1.Deployment
	err := utils.GetDeploymentWithBackoff(ctx, a.Client, va.Name, va.Namespace, &deploy)
	if err != nil {
		return 0, fmt.Errorf("failed to get Deployment %s/%s: %w", va.Namespace, va.Name, err)
	}

	// Prefer status replicas (actual current state)
	if deploy.Status.Replicas >= 0 {
		return deploy.Status.Replicas, nil
	}

	// Fallback to spec if status not ready
	if deploy.Spec.Replicas != nil {
		return *deploy.Spec.Replicas, nil
	}

	// Final fallback
	return 1, nil
}

func (a *Actuator) EmitMetrics(ctx context.Context, VariantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	// Emit replica metrics with real-time data for external autoscalers
	// Check if we have desired allocations
	if len(VariantAutoscaling.Status.DesiredOptimizedAllocs) == 0 {
		logger.Log.Info("Skipping EmitReplicaMetrics for variantAutoscaling - no desired allocations", "variantAutoscaling-name", VariantAutoscaling.Name)
		return nil
	}

	// Emit metrics for all allocations with variantID labels
	for _, desiredAlloc := range VariantAutoscaling.Status.DesiredOptimizedAllocs {
		if desiredAlloc.NumReplicas < 0 {
			logger.Log.Info("Skipping EmitReplicaMetrics for allocation - NumReplicas is negative",
				"variant-name", VariantAutoscaling.Name,
				"variant-id", desiredAlloc.VariantID,
				"namespace", VariantAutoscaling.Namespace)
			continue
		}

		// Get real current replicas from Deployment (not stale VariantAutoscaling status)
		currentReplicas, err := a.getCurrentDeploymentReplicas(ctx, VariantAutoscaling)
		if err != nil {
			logger.Log.Warn("Could not get current deployment replicas, using VariantAutoscaling status",
				"error", err,
				"variant-name", VariantAutoscaling.Name,
				"variant-id", desiredAlloc.VariantID,
				"namespace", VariantAutoscaling.Namespace)
			// fallback to current allocation matching this variantID
			currentReplicas = 0
			for _, currentAlloc := range VariantAutoscaling.Status.CurrentAllocs {
				if currentAlloc.VariantID == desiredAlloc.VariantID {
					currentReplicas = int32(currentAlloc.NumReplicas)
					break
				}
			}
		}

		if err := a.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			VariantAutoscaling,
			currentReplicas,                 // Real current from Deployment
			int32(desiredAlloc.NumReplicas), // Inferno's optimization target
			desiredAlloc.Accelerator,
			desiredAlloc.VariantID, // Include variantID as label
		); err != nil {
			logger.Log.Error(err, "Failed to emit optimization signals for allocation",
				"variant-name", VariantAutoscaling.Name,
				"variant-id", desiredAlloc.VariantID,
				"namespace", VariantAutoscaling.Namespace)
			// Don't fail the reconciliation for metric emission errors
			// Metrics are critical for HPA, but emission failures shouldn't break core functionality
			continue
		}
		logger.Log.Debug("EmitReplicaMetrics completed for allocation",
			"variant-name", VariantAutoscaling.Name,
			"variant-id", desiredAlloc.VariantID,
			"namespace", VariantAutoscaling.Namespace,
			"current-replicas", currentReplicas,
			"desired-replicas", desiredAlloc.NumReplicas,
			"accelerator", desiredAlloc.Accelerator)
	}
	return nil
}
