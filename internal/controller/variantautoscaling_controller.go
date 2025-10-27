/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	actuator "github.com/llm-d-incubation/workload-variant-autoscaler/internal/actuator"
	collector "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/metrics"
	analyzer "github.com/llm-d-incubation/workload-variant-autoscaler/internal/modelanalyzer"
	variantAutoscalingOptimizer "github.com/llm-d-incubation/workload-variant-autoscaler/internal/optimizer"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	inferno "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/core"
	infernoManager "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/manager"
	infernoSolver "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/solver"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// VariantAutoscalingReconciler reconciles a variantAutoscaling object
type VariantAutoscalingReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	PromAPI                 promv1.API
	MetricsCache            *collector.ModelMetricsCache       // Cache for model-level Prometheus metrics
	ScaleToZeroMetricsCache *collector.ScaleToZeroMetricsCache // Cache for scale-to-zero internal metrics
}

// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;update;patch;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update;list;watch

const (
	configMapName      = "workload-variant-autoscaler-variantautoscaling-config"
	configMapNamespace = "workload-variant-autoscaler-system"
)

func initMetricsEmitter() {
	logger.Log.Info("Creating metrics emitter instance")
	// Force initialization of metrics by creating a metrics emitter
	_ = metrics.NewMetricsEmitter()
	logger.Log.Info("Metrics emitter created successfully")
}

func (r *VariantAutoscalingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	interval, err := r.readOptimizationConfig(ctx)
	if err != nil {
		logger.Log.Error(err, "Unable to read optimization config")
		return ctrl.Result{}, err
	}

	// default requeue duration
	requeueDuration := 60 * time.Second

	if interval != "" {
		if requeueDuration, err = time.ParseDuration(interval); err != nil {
			return ctrl.Result{}, err
		}
	}

	if strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true") {
		logger.Log.Info("Scaling to zero is enabled!")
	}

	// TODO: decide on whether to keep accelerator properties (device name, cost) in same configMap, provided by administrator
	acceleratorCm, err := r.readAcceleratorConfig(ctx, "accelerator-unit-costs", configMapNamespace)
	if err != nil {
		logger.Log.Error(err, "unable to read accelerator configMap, skipping optimizing")
		return ctrl.Result{}, err
	}

	serviceClassCm, err := r.readServiceClassConfig(ctx, "service-classes-config", configMapNamespace)
	if err != nil {
		logger.Log.Error(err, "unable to read serviceclass configMap, skipping optimizing")
		return ctrl.Result{}, err
	}

	// Read scale-to-zero configuration (optional - falls back to global defaults if not found)
	scaleToZeroConfigData, err := r.readScaleToZeroConfig(ctx, "model-scale-to-zero-config", configMapNamespace)
	if err != nil {
		logger.Log.Error(err, "unable to read scale-to-zero configMap, using global defaults")
		scaleToZeroConfigData = make(utils.ScaleToZeroConfigData)
	}

	var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	if err := r.List(ctx, &variantAutoscalingList); err != nil {
		logger.Log.Error(err, "unable to list variantAutoscaling resources")
		return ctrl.Result{}, err
	}

	activeVAs := filterActiveVariantAutoscalings(variantAutoscalingList.Items)

	if len(activeVAs) == 0 {
		logger.Log.Info("No active VariantAutoscalings found, skipping optimization")
		return ctrl.Result{}, nil
	}

	// Check for variants using default variantCost and log warnings
	if len(activeVAs) > 1 {
		variantsWithDefaultCost := []string{}
		for _, va := range activeVAs {
			if va.Spec.VariantCost == "" || va.Spec.VariantCost == "10" {
				variantsWithDefaultCost = append(variantsWithDefaultCost, va.Name)
			}
		}
		if len(variantsWithDefaultCost) > 0 {
			logger.Log.Info("Warning: Multiple variants detected with some using default variantCost",
				"totalVariants", len(activeVAs),
				"variantsUsingDefault", len(variantsWithDefaultCost),
				"variantNames", strings.Join(variantsWithDefaultCost, ", "),
				"recommendation", "Set explicit variantCost for accurate cost comparisons")
		}
	}

	// WVA operates in unlimited mode - no cluster inventory collection needed
	systemData := utils.CreateSystemData(acceleratorCm, serviceClassCm)

	updateList, vaMap, allAnalyzerResponses, err := r.prepareVariantAutoscalings(ctx, activeVAs, acceleratorCm, serviceClassCm, systemData, scaleToZeroConfigData)
	if err != nil {
		logger.Log.Error(err, "failed to prepare variant autoscalings")
		return ctrl.Result{}, err
	}

	// analyze
	system := inferno.NewSystem()
	optimizerSpec := system.SetFromSpec(&systemData.Spec)
	optimizer := infernoSolver.NewOptimizerFromSpec(optimizerSpec)
	manager := infernoManager.NewManager(system, optimizer)

	modelAnalyzer := analyzer.NewModelAnalyzer(system)
	for _, s := range system.Servers() {
		// Safely get VA from map with nil check
		va, ok := vaMap[s.Name()]
		if !ok || va == nil {
			logger.Log.Error(nil, "VA not found in map for server", "serverName", s.Name())
			continue
		}

		modelAnalyzeResponse := modelAnalyzer.AnalyzeModel(ctx, *va)
		if len(modelAnalyzeResponse.Allocations) == 0 {
			logger.Log.Info("No potential allocations found for server - ", "serverName: ", s.Name())
			continue
		}
		allAnalyzerResponses[s.Name()] = modelAnalyzeResponse
	}
	logger.Log.Debug("System data prepared for optimization: - ", utils.MarshalStructToJsonString(systemData.Spec.Capacity))
	logger.Log.Debug("System data prepared for optimization: - ", utils.MarshalStructToJsonString(systemData.Spec.Accelerators))
	logger.Log.Debug("System data prepared for optimization: - ", utils.MarshalStructToJsonString(systemData.Spec.ServiceClasses))
	logger.Log.Debug("System data prepared for optimization: - ", utils.MarshalStructToJsonString(systemData.Spec.Models))
	logger.Log.Debug("System data prepared for optimization: - ", utils.MarshalStructToJsonString(systemData.Spec.Optimizer))
	logger.Log.Debug("System data prepared for optimization: - ", utils.MarshalStructToJsonString(systemData.Spec.Servers))

	engine := variantAutoscalingOptimizer.NewVariantAutoscalingsEngine(manager, system)

	optimizedAllocation, err := engine.Optimize(ctx, *updateList, allAnalyzerResponses, &scaleToZeroConfigData, r.ScaleToZeroMetricsCache)
	if err != nil {
		logger.Log.Error(err, "unable to perform model optimization, skipping this iteration")

		// Update OptimizationReady condition to False for all VAs in the update list
		for i := range updateList.Items {
			va := &updateList.Items[i]

			// Fetch fresh copy to avoid status update conflicts
			var freshVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			if err := r.Get(ctx, client.ObjectKeyFromObject(va), &freshVA); err != nil {
				logger.Log.Error(err, "failed to fetch fresh VA for status update",
					"name", va.Name, "namespace", va.Namespace)
				continue
			}

			llmdVariantAutoscalingV1alpha1.SetCondition(&freshVA,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
				fmt.Sprintf("Optimization failed: %v", err))

			if statusErr := r.Status().Update(ctx, &freshVA); statusErr != nil {
				logger.Log.Error(statusErr, "failed to update status condition after optimization failure",
					"variantAutoscaling", freshVA.Name)
			}
		}

		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	logger.Log.Debug("Optimization completed successfully, emitting optimization metrics")
	logger.Log.Debug("Optimized allocation map - ", "numKeys: ", len(optimizedAllocation), ", updateList_count: ", len(updateList.Items))
	for key, value := range optimizedAllocation {
		logger.Log.Debug("Optimized allocation entry - ", "key: ", key, ", value: ", value)
	}

	if err := r.applyOptimizedAllocations(ctx, updateList, optimizedAllocation, allAnalyzerResponses); err != nil {
		// If we fail to apply optimized allocations, we log the error
		// In next reconcile, the controller will retry.
		logger.Log.Error(err, "failed to apply optimized allocations")
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	return ctrl.Result{RequeueAfter: requeueDuration}, nil
}

// filterActiveVariantAutoscalings returns only those VAs not marked for deletion.
func filterActiveVariantAutoscalings(items []llmdVariantAutoscalingV1alpha1.VariantAutoscaling) []llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	active := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(items))
	for _, va := range items {
		if va.DeletionTimestamp.IsZero() {
			active = append(active, va)
		} else {
			logger.Log.Info("skipping deleted variantAutoscaling - ", "variantAutoscaling-name: ", va.Name)
		}
	}
	return active
}

// addVariantWithFallbackAllocation adds a variant to the update list with fallback allocation
// when metrics or optimization data is unavailable. This ensures metrics are always emitted
// even when optimization cannot proceed normally.
//
// Fallback logic:
// 1. Respect replica bounds (minReplicas, maxReplicas)
// 2. If scale-to-zero is disabled and no minReplicas: set 1 replica for cheapest variant only
// 3. If scale-to-zero is enabled AND aggregate metrics available: scale to zero only if load == 0
// 4. If aggregate metrics not available: can set to 0 for all variants
func addVariantWithFallbackAllocation(
	updateVA *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deploy *appsv1.Deployment,
	reason string,
	message string,
	updateList *llmdVariantAutoscalingV1alpha1.VariantAutoscalingList,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
	modelName string,
	aggregateLoad *float64,
	isCheapestVariant bool,
) {
	// Log warning about using fallback allocation
	logger.Log.Warnw("Using fallback allocation for variant - optimization data unavailable",
		"variant", updateVA.Name,
		"namespace", updateVA.Namespace,
		"model", modelName,
		"reason", reason)

	// Get current replicas from deployment
	var currentReplicas int32
	if deploy != nil {
		if deploy.Status.Replicas > 0 {
			currentReplicas = deploy.Status.Replicas
		} else if deploy.Spec.Replicas != nil {
			currentReplicas = *deploy.Spec.Replicas
		}
	}

	// Determine desired replicas based on scale-to-zero config and aggregate metrics
	var desiredReplicas int32
	scaleToZeroEnabled := utils.IsScaleToZeroEnabled(scaleToZeroConfigData, modelName)

	if !scaleToZeroEnabled {
		// Scale-to-zero is disabled
		// If no minReplicas set and this is the cheapest variant, maintain at least 1 replica
		if updateVA.Spec.MinReplicas == nil {
			if isCheapestVariant {
				desiredReplicas = max(1, currentReplicas)
				message = fmt.Sprintf("%s. Scale-to-zero disabled, maintaining cheapest variant with min 1 replica", message)
			} else {
				desiredReplicas = currentReplicas // Other variants can be 0
			}
		} else {
			// MinReplicas is set, respect it
			desiredReplicas = currentReplicas
		}
	} else {
		// Scale-to-zero is enabled
		if aggregateLoad != nil {
			// Aggregate metrics available - check load
			if *aggregateLoad > 0 {
				// There is load - maintain current replicas
				desiredReplicas = currentReplicas
				message = fmt.Sprintf("%s. Scale-to-zero enabled but load > 0 (%.2f), maintaining current replicas", message, *aggregateLoad)
			} else {
				// No load - can scale to zero
				desiredReplicas = 0
				message = fmt.Sprintf("%s. Scale-to-zero enabled and load == 0, scaling to zero", message)
			}
		} else {
			// Aggregate metrics not available - safe to scale to zero
			desiredReplicas = 0
			message = fmt.Sprintf("%s. Scale-to-zero enabled, aggregate metrics unavailable, scaling to zero", message)
		}
	}

	// Apply replica bounds (clamp to [minReplicas, maxReplicas])
	if updateVA.Spec.MinReplicas != nil {
		minReplicas := *updateVA.Spec.MinReplicas
		if desiredReplicas < minReplicas {
			logger.Log.Info("Clamping desired replicas to minReplicas",
				"variant", updateVA.Name,
				"original", desiredReplicas,
				"clamped", minReplicas)
			desiredReplicas = minReplicas
		}
	}
	if updateVA.Spec.MaxReplicas != nil {
		maxReplicas := *updateVA.Spec.MaxReplicas
		if desiredReplicas > maxReplicas {
			logger.Log.Info("Clamping desired replicas to maxReplicas",
				"variant", updateVA.Name,
				"original", desiredReplicas,
				"clamped", maxReplicas)
			desiredReplicas = maxReplicas
		}
	}

	// Set current allocation
	updateVA.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
		NumReplicas: currentReplicas,
	}

	// Set desired allocation with computed fallback value
	updateVA.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
		NumReplicas: desiredReplicas,
		LastRunTime: metav1.Now(),
	}

	// Mark optimization as succeeded with fallback explanation
	llmdVariantAutoscalingV1alpha1.SetCondition(updateVA,
		llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
		metav1.ConditionTrue,
		reason,
		message)

	// Add to update list for metric emission
	updateList.Items = append(updateList.Items, *updateVA)

	logger.Log.Info("Added variant with fallback allocation for metric emission",
		"variant", updateVA.Name,
		"currentReplicas", currentReplicas,
		"desiredReplicas", desiredReplicas,
		"scaleToZeroEnabled", scaleToZeroEnabled,
		"isCheapest", isCheapestVariant,
		"reason", reason)
}

// max returns the maximum of two int32 values
func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// isCheapestVariantForModel determines if the given variant is the cheapest among all variants for the same model.
// Cheapest is determined by accelerator count (fewer accelerators = cheaper).
func isCheapestVariantForModel(
	currentVariant *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	modelName string,
) bool {
	if currentVariant == nil {
		return false
	}

	minAcceleratorCount := currentVariant.Spec.AcceleratorCount
	cheapestVariantID := currentVariant.Spec.VariantID

	// Find the variant with minimum accelerator count for this model
	for _, va := range allVariants {
		if va.Spec.ModelID != modelName {
			continue // Different model
		}
		if va.Spec.AcceleratorCount < minAcceleratorCount {
			minAcceleratorCount = va.Spec.AcceleratorCount
			cheapestVariantID = va.Spec.VariantID
		} else if va.Spec.AcceleratorCount == minAcceleratorCount {
			// If same count, choose by variant ID (deterministic)
			if va.Spec.VariantID < cheapestVariantID {
				cheapestVariantID = va.Spec.VariantID
			}
		}
	}

	return currentVariant.Spec.VariantID == cheapestVariantID
}

// prepareVariantAutoscalings collects and prepares all data for optimization.
func (r *VariantAutoscalingReconciler) prepareVariantAutoscalings(
	ctx context.Context,
	activeVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	acceleratorCm map[string]map[string]string,
	serviceClassCm map[string]string,
	systemData *infernoConfig.SystemData,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
) (*llmdVariantAutoscalingV1alpha1.VariantAutoscalingList, map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, map[string]*interfaces.ModelAnalyzeResponse, error) {
	var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	allAnalyzerResponses := make(map[string]*interfaces.ModelAnalyzeResponse)
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for _, va := range activeVAs {
		modelName := va.Spec.ModelID
		if modelName == "" {
			logger.Log.Warn("variantAutoscaling missing modelName, using fallback allocation for metric emission - ", "variantAutoscaling-name: ", va.Name)
			// Get the VA to emit fallback metrics
			var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVA); err == nil {
				isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
				addVariantWithFallbackAllocation(&updateVA, nil,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
					"ModelID is empty - cannot optimize",
					&updateList,
					scaleToZeroConfigData,
					modelName,
					nil, // No aggregate load available
					isCheapest)
			}
			continue
		}

		entry, className, err := utils.FindModelSLO(serviceClassCm, modelName)
		if err != nil {
			logger.Log.Error(err, "failed to locate SLO for model, using fallback allocation for metric emission - ", "variantAutoscaling-name: ", va.Name, "modelName: ", modelName)
			// Get the VA to emit fallback metrics
			var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVA); err == nil {
				isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
				addVariantWithFallbackAllocation(&updateVA, nil,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
					fmt.Sprintf("SLO not found for model %s", modelName),
					&updateList,
					scaleToZeroConfigData,
					modelName,
					nil, // No aggregate load available
					isCheapest)
			}
			continue
		}
		logger.Log.Info("Found SLO for model - ", "model: ", modelName, ", class: ", className, ", slo-tpot: ", entry.SLOTPOT, ", slo-ttft: ", entry.SLOTTFT)

		if err := utils.AddVariantProfileToSystemData(systemData,
			modelName,
			va.Spec.Accelerator,
			va.Spec.AcceleratorCount,
			&va.Spec.VariantProfile); err != nil {
			logger.Log.Error(err, "failed to add variant profile to system data, using fallback allocation for metric emission", "variantAutoscaling", va.Name)
			// Get the VA to emit fallback metrics
			var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVA); err == nil {
				isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
				addVariantWithFallbackAllocation(&updateVA, nil,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
					"Failed to add variant profile to system data",
					&updateList,
					scaleToZeroConfigData,
					modelName,
					nil, // No aggregate load available
					isCheapest)
			}
			continue
		}

		var deploy appsv1.Deployment
		err = utils.GetDeploymentWithBackoff(ctx, r.Client, va.Name, va.Namespace, &deploy)
		if err != nil {
			logger.Log.Error(err, "failed to get Deployment after retries, using fallback allocation for metric emission - ", "variantAutoscaling-name: ", va.Name)
			// Get the VA to emit fallback metrics (deployment not found, so use 0 replicas)
			var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
			if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVA); err == nil {
				isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
				addVariantWithFallbackAllocation(&updateVA, nil,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
					"Deployment not found - using 0 replicas as fallback",
					&updateList,
					scaleToZeroConfigData,
					modelName,
					nil, // No aggregate load available
					isCheapest)
			}
			continue
		}

		var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		err = utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, deploy.Name, deploy.Namespace, &updateVA)
		if err != nil {
			logger.Log.Error(err, "unable to get variantAutoscaling for deployment, using fallback allocation for metric emission - ", "deployment-name: ", deploy.Name, ", namespace: ", deploy.Namespace)
			// This is unusual - deployment exists but VA doesn't (should not happen normally)
			// Skip this one as we can't emit metrics without the VA object
			continue
		}

		// Validate and log the relationship between variant_name and variant_id
		// This helps users understand the dual-identifier pattern used in Prometheus metrics
		utils.ValidateVariantAutoscalingName(&updateVA)

		// Set ownerReference early, before metrics validation, to ensure it's always set
		// This ensures the VA will be garbage collected when the Deployment is deleted
		if !metav1.IsControlledBy(&updateVA, &deploy) {
			original := updateVA.DeepCopy()
			err := controllerutil.SetControllerReference(&deploy, &updateVA, r.Scheme, controllerutil.WithBlockOwnerDeletion(false))
			if err != nil {
				logger.Log.Error(err, "failed to set ownerReference - ", "variantAutoscaling-name: ", updateVA.Name)
				continue
			}

			// Patch metadata change (ownerReferences)
			patch := client.MergeFrom(original)
			if err := r.Patch(ctx, &updateVA, patch); err != nil {
				logger.Log.Error(err, "failed to patch ownerReference - ", "variantAutoscaling-name: ", updateVA.Name)
				continue
			}
			logger.Log.Info("Set ownerReference on VariantAutoscaling - ", "variantAutoscaling-name: ", updateVA.Name, ", owner: ", deploy.Name)
		}

		// Validate metrics availability before collecting metrics
		metricsValidation := collector.ValidateMetricsAvailability(ctx, r.PromAPI, modelName, deploy.Namespace)

		// Update MetricsAvailable condition based on validation result
		if metricsValidation.Available {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionTrue,
				metricsValidation.Reason,
				metricsValidation.Message)
		} else {
			// Metrics unavailable - emit fallback metrics to prevent external autoscalers from breaking
			logger.Log.Warnw("Metrics unavailable, using fallback allocation for metric emission",
				"variant", updateVA.Name,
				"namespace", updateVA.Namespace,
				"model", modelName,
				"reason", metricsValidation.Reason,
				"troubleshooting", metricsValidation.Message)

			// Set MetricsAvailable condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionFalse,
				metricsValidation.Reason,
				metricsValidation.Message)

			// Add to updateList with fallback allocation for metric emission
			isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
			addVariantWithFallbackAllocation(&updateVA, &deploy,
				llmdVariantAutoscalingV1alpha1.ReasonMetricsUnavailable,
				fmt.Sprintf("Metrics unavailable (%s)", metricsValidation.Reason),
				&updateList,
				scaleToZeroConfigData,
				modelName,
				nil, // No aggregate load available
				isCheapest)
			continue
		}

		// Get retention period for this model from scale-to-zero config
		retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

		// Collect allocation and scale-to-zero metrics for this variant
		allocation, err := collector.AddMetricsToOptStatus(ctx, &updateVA, deploy, r.PromAPI, r.ScaleToZeroMetricsCache, retentionPeriod)
		if err != nil {
			logger.Log.Error(err, "unable to collect allocation data, using fallback allocation for metric emission")
			// Add to updateList with fallback allocation for metric emission
			isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
			addVariantWithFallbackAllocation(&updateVA, &deploy,
				llmdVariantAutoscalingV1alpha1.ReasonMetricsUnavailable,
				fmt.Sprintf("Failed to collect allocation data: %v", err),
				&updateList,
				scaleToZeroConfigData,
				modelName,
				nil, // No aggregate load available
				isCheapest)
			continue
		}

		// Update status with allocation (metrics are passed separately in refactored architecture)
		updateVA.Status.CurrentAlloc = allocation

		// Collect aggregate metrics (shared across all variants of this model)
		// Use cache to avoid redundant Prometheus queries for same model
		load, ttftAvg, itlAvg, err := collector.CollectAggregateMetricsWithCache(ctx, modelName, deploy.Namespace, r.PromAPI, r.MetricsCache)
		if err != nil {
			logger.Log.Error(err, "unable to fetch aggregate metrics, using fallback allocation for metric emission")
			// Add to updateList with fallback allocation for metric emission
			isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
			addVariantWithFallbackAllocation(&updateVA, &deploy,
				llmdVariantAutoscalingV1alpha1.ReasonMetricsUnavailable,
				fmt.Sprintf("Failed to collect aggregate metrics: %v", err),
				&updateList,
				scaleToZeroConfigData,
				modelName,
				nil, // No aggregate load available
				isCheapest)
			continue
		}

		// Update status with collected data (allocation already set by AddMetricsToOptStatus)
		// Extract metrics to internal structure (all metrics passed separately from Prometheus)
		metrics, err := interfaces.NewVariantMetrics(load, ttftAvg, itlAvg)
		if err != nil {
			logger.Log.Error(err, "failed to parse variant metrics, using fallback allocation for metric emission")
			// Add to updateList with fallback allocation for metric emission
			// Extract load value from LoadProfile
			var loadValue *float64
			if arrivalRate, parseErr := strconv.ParseFloat(load.ArrivalRate, 64); parseErr == nil {
				loadValue = &arrivalRate
			}
			isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
			addVariantWithFallbackAllocation(&updateVA, &deploy,
				llmdVariantAutoscalingV1alpha1.ReasonMetricsUnavailable,
				fmt.Sprintf("Failed to parse variant metrics: %v", err),
				&updateList,
				scaleToZeroConfigData,
				modelName,
				loadValue, // We have load but failed to parse full metrics
				isCheapest)
			continue
		}

		// Add server info with both metrics and scale-to-zero configuration
		if err := utils.AddServerInfoToSystemData(systemData, &updateVA, className, metrics, scaleToZeroConfigData); err != nil {
			logger.Log.Warn("variantAutoscaling bad deployment server data, using fallback allocation for metric emission")
			// Add to updateList with fallback allocation for metric emission
			// Extract load value from LoadProfile
			var loadValue *float64
			if arrivalRate, parseErr := strconv.ParseFloat(load.ArrivalRate, 64); parseErr == nil {
				loadValue = &arrivalRate
			}
			isCheapest := isCheapestVariantForModel(&updateVA, activeVAs, modelName)
			addVariantWithFallbackAllocation(&updateVA, &deploy,
				llmdVariantAutoscalingV1alpha1.ReasonMetricsUnavailable,
				fmt.Sprintf("Failed to add server info: %v", err),
				&updateList,
				scaleToZeroConfigData,
				modelName,
				loadValue, // We have load and metrics but validation failed
				isCheapest)
			continue
		}

		vaFullName := utils.FullName(va.Name, va.Namespace)
		updateList.Items = append(updateList.Items, updateVA)
		vaMap[vaFullName] = &va
	}
	return &updateList, vaMap, allAnalyzerResponses, nil
}

// applyOptimizedAllocations applies the optimized allocation to all VariantAutoscaling resources.
func (r *VariantAutoscalingReconciler) applyOptimizedAllocations(
	ctx context.Context,
	updateList *llmdVariantAutoscalingV1alpha1.VariantAutoscalingList,
	optimizedAllocation map[string]llmdVariantAutoscalingV1alpha1.OptimizedAlloc,
	allAnalyzerResponses map[string]*interfaces.ModelAnalyzeResponse,
) error {
	logger.Log.Debug("Optimization metrics emitted, starting to process variants - ", "variant_count: ", len(updateList.Items))

	// Create metrics emitter for prediction metrics
	metricsEmitter := metrics.NewMetricsEmitter()

	for i := range updateList.Items {
		va := &updateList.Items[i]
		optimizedAlloc, hasOptimizedAlloc := optimizedAllocation[va.Name]
		logger.Log.Debug("Processing variant - ", "index: ", i, ", variantAutoscaling-name: ", va.Name, ", namespace: ", va.Namespace, ", has_optimized_alloc: ", hasOptimizedAlloc)

		// Emit prediction metrics for the SELECTED allocation only (when available)
		// This is done AFTER optimization selects which accelerator to use
		if hasOptimizedAlloc {
			if analyzerResponse, found := allAnalyzerResponses[va.Name]; found && analyzerResponse != nil {
				// Get the selected accelerator from the variant spec
				selectedAccelerator := va.Spec.Accelerator

				// Get the allocation data for the selected accelerator only
				if acceleratorAlloc, acceleratorFound := analyzerResponse.Allocations[selectedAccelerator]; acceleratorFound {
					if acceleratorAlloc != nil && acceleratorAlloc.Allocation != nil {
						allocData := acceleratorAlloc.Allocation.AllocationData()

						// Convert from milliseconds to seconds
						ttftSeconds := float64(allocData.TTFTAverage) / 1000.0
						itlSeconds := float64(allocData.ITLAverage) / 1000.0

						// Emit metrics with correct VariantID (business ID, not Kubernetes UID)
						if err := metricsEmitter.EmitPredictionMetrics(ctx, va, va.Spec.ModelID, ttftSeconds, itlSeconds, selectedAccelerator); err != nil {
							logger.Log.Error(err, "Failed to emit prediction metrics",
								"variantName", va.Name,
								"modelID", va.Spec.ModelID,
								"accelerator", selectedAccelerator)
						} else {
							logger.Log.Debug("Successfully emitted prediction metrics",
								"variantName", va.Name,
								"accelerator", selectedAccelerator,
								"ttft_seconds", ttftSeconds,
								"itl_seconds", itlSeconds)
						}
					}
				} else {
					logger.Log.Debug("No allocation found for selected accelerator",
						"variantName", va.Name,
						"accelerator", selectedAccelerator)
				}
			}
		}

		// Fetch the latest version from API server
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Log.Error(err, "failed to get latest VariantAutoscaling from API server: ", "variantAutoscaling-name: ", va.Name)
			continue
		}

		// Note: ownerReference is now set earlier in prepareVariantAutoscalings
		// This ensures it's set even if metrics aren't available yet

		// Initialize CurrentAlloc from actual deployment replicas
		// This ensures Status.CurrentAlloc reflects reality, not stale data
		var currentReplicas int32
		var deploy appsv1.Deployment
		if err := utils.GetDeploymentWithBackoff(ctx, r.Client, va.Name, va.Namespace, &deploy); err != nil {
			// Deployment doesn't exist yet (first reconciliation) or error occurred
			// Use existing status value, or 0 if never initialized
			logger.Log.Debug("Could not get deployment for CurrentAlloc initialization, using existing status",
				"variant", va.Name, "error", err)
			currentReplicas = va.Status.CurrentAlloc.NumReplicas
		} else {
			// Use actual deployment replicas
			// Prefer Status.Replicas (current state), but fall back to Spec.Replicas if Status not populated yet
			if deploy.Status.Replicas > 0 {
				currentReplicas = deploy.Status.Replicas
			} else if deploy.Spec.Replicas != nil {
				currentReplicas = *deploy.Spec.Replicas
			}
			logger.Log.Debug("Initialized CurrentAlloc from deployment",
				"variant", va.Name, "replicas", currentReplicas)
		}

		updateVa.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
			NumReplicas: currentReplicas,
		}

		// Use optimized allocation if available, otherwise use fallback (0 replicas)
		// This ensures metrics are always emitted, even for zero-traffic scenarios
		if hasOptimizedAlloc {
			updateVa.Status.DesiredOptimizedAlloc = optimizedAlloc
			logger.Log.Debug("Using optimized allocation",
				"variantName", va.Name,
				"replicas", optimizedAlloc.NumReplicas)
		} else {
			// Fallback: No traffic/metrics available, use minimum allocation
			// This ensures KEDA can still query metrics for scale-to-zero scenarios
			updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
				NumReplicas: 0,
			}
			logger.Log.Debug("No optimized allocation found, using fallback (0 replicas)",
				"variantName", va.Name,
				"accelerator", va.Spec.Accelerator)
		}

		updateVa.Status.Actuation.Applied = false // No longer directly applying changes

		// Copy existing conditions from updateList (includes MetricsAvailable condition set during preparation)
		// This ensures we don't lose the MetricsAvailable condition when fetching fresh copy from API
		// Always copy, even if empty, to preserve conditions set during prepareVariantAutoscalings
		updateVa.Status.Conditions = va.Status.Conditions

		// Set OptimizationReady condition
		desiredAlloc := updateVa.Status.DesiredOptimizedAlloc
		if hasOptimizedAlloc {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				fmt.Sprintf("Optimization completed: %d replicas on %s",
					desiredAlloc.NumReplicas,
					updateVa.Spec.Accelerator)) // Use spec field (single-variant architecture)
		} else {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				fmt.Sprintf("No metrics available, using fallback: %d replicas on %s",
					desiredAlloc.NumReplicas,
					updateVa.Spec.Accelerator))
		}

		act := actuator.NewActuator(r.Client)

		// ALWAYS emit optimization signals for external autoscalers (KEDA, HPA, etc.)
		// This is critical for scale-to-zero scenarios where metrics must exist even with no traffic
		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			logger.Log.Error(err, "failed to emit optimization signals for external autoscalers - ", "variant: ", updateVa.Name)
		} else {
			logger.Log.Debug("Successfully emitted optimization signals for external autoscalers - ", "variant: ", updateVa.Name)
			updateVa.Status.Actuation.Applied = true // Signals emitted successfully
		}

		if err := utils.UpdateStatusWithBackoff(ctx, r.Client, &updateVa, utils.StandardBackoff, "VariantAutoscaling"); err != nil {
			logger.Log.Error(err, "failed to patch status for variantAutoscaling after retries - ", "variantAutoscaling-name: ", updateVa.Name)
			continue
		}
	}

	logger.Log.Debug("Completed variant processing loop")

	// Log summary of reconciliation
	if len(updateList.Items) > 0 {
		logger.Log.Info("Reconciliation completed - ",
			"variants_processed: ", len(updateList.Items),
			", optimization_successful: ", true)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VariantAutoscalingReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Initialize metrics
	initMetricsEmitter()

	// Configure Prometheus client using flexible configuration with TLS support
	promConfig, err := r.getPrometheusConfig(context.Background())
	if err != nil {
		return fmt.Errorf("failed to get Prometheus configuration: %w", err)
	}

	// ensure we have a valid configuration
	if promConfig == nil {
		return fmt.Errorf("no Prometheus configuration found - this should not happen")
	}

	// Always validate TLS configuration since HTTPS is required
	if err := utils.ValidateTLSConfig(promConfig); err != nil {
		logger.Log.Error(err, "TLS configuration validation failed - HTTPS is required")
		return fmt.Errorf("TLS configuration validation failed: %w", err)
	}

	logger.Log.Info("Initializing Prometheus client -> ", "address: ", promConfig.BaseURL, " tls_enabled: true")

	// Create Prometheus client with TLS support
	promClientConfig, err := utils.CreatePrometheusClientConfig(promConfig)
	if err != nil {
		return fmt.Errorf("failed to create prometheus client config: %w", err)
	}

	promClient, err := api.NewClient(*promClientConfig)
	if err != nil {
		return fmt.Errorf("failed to create prometheus client: %w", err)
	}

	r.PromAPI = promv1.NewAPI(promClient)

	// Initialize scale-to-zero metrics cache for storing internal per-model scale-to-zero metrics
	r.ScaleToZeroMetricsCache = collector.NewScaleToZeroMetricsCache()
	logger.Log.Info("Scale-to-zero metrics cache initialized")

	// Validate that the API is working by testing a simple query with retry logic
	if err := utils.ValidatePrometheusAPI(context.Background(), r.PromAPI); err != nil {
		logger.Log.Error(err, "CRITICAL: Failed to connect to Prometheus - Inferno requires Prometheus connectivity for autoscaling decisions")
		return fmt.Errorf("critical: failed to validate Prometheus API connection - autoscaling functionality requires Prometheus: %w", err)
	}
	logger.Log.Info("Prometheus client and API wrapper initialized and validated successfully")

	// Read reconciliation interval from ConfigMap to calculate optimal cache TTL
	// This ensures cache expires between reconciliation loops for fresh Prometheus data
	intervalStr, err := r.readOptimizationConfig(context.Background())
	if err != nil {
		logger.Log.Warn("Failed to read optimization config, using default reconciliation interval",
			"error", err.Error())
		intervalStr = "" // Will default to 60s below
	}

	// Parse reconciliation interval (default 60s if not set)
	reconciliationInterval := 60 * time.Second
	if intervalStr != "" {
		if parsedInterval, parseErr := time.ParseDuration(intervalStr); parseErr != nil {
			logger.Log.Warn("Failed to parse reconciliation interval, using default",
				"configuredInterval", intervalStr,
				"error", parseErr.Error(),
				"default", reconciliationInterval.String())
		} else {
			reconciliationInterval = parsedInterval
		}
	}

	// Calculate cache TTL as half of reconciliation interval
	// This guarantees cache expires between reconciliation loops, ensuring fresh data
	// while maintaining caching benefit for multiple VAs within same reconciliation batch
	cacheTTL := reconciliationInterval / 2

	// Apply minimum TTL of 5 seconds to prevent excessive Prometheus queries
	// if reconciliation interval is configured very short (< 10s)
	minCacheTTL := 5 * time.Second
	if cacheTTL < minCacheTTL {
		logger.Log.Warn("Calculated cache TTL too short, using minimum",
			"calculated", cacheTTL.String(),
			"minimum", minCacheTTL.String(),
			"reconciliationInterval", reconciliationInterval.String())
		cacheTTL = minCacheTTL
	}

	r.MetricsCache = collector.NewModelMetricsCache(cacheTTL)
	logger.Log.Info("Model metrics cache initialized with dynamic TTL",
		"cacheTTL", cacheTTL.String(),
		"reconciliationInterval", reconciliationInterval.String(),
		"ratio", "TTL = interval / 2")

	//logger.Log.Info("Prometheus client initialized (validation skipped)")

	return ctrl.NewControllerManagedBy(mgr).
		For(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}).
		// Watch the specific ConfigMap to trigger global reconcile
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() == configMapName && obj.GetNamespace() == configMapNamespace {
					return []reconcile.Request{{}}
				}
				return nil
			}),
			// Predicate to filter only the target configmap
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetName() == configMapName && obj.GetNamespace() == configMapNamespace
			})),
		).
		// Watch the model-scale-to-zero-config ConfigMap to trigger reconcile when scale-to-zero config changes
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() == "model-scale-to-zero-config" && obj.GetNamespace() == configMapNamespace {
					return []reconcile.Request{{}}
				}
				return nil
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetName() == "model-scale-to-zero-config" && obj.GetNamespace() == configMapNamespace
			})),
		).
		Named("variantAutoscaling").
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}

func (r *VariantAutoscalingReconciler) readServiceClassConfig(ctx context.Context, cmName, cmNamespace string) (map[string]string, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, cmName, cmNamespace, &cm)
	if err != nil {
		return nil, err
	}
	return cm.Data, nil
}

func (r *VariantAutoscalingReconciler) readAcceleratorConfig(ctx context.Context, cmName, cmNamespace string) (map[string]map[string]string, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, cmName, cmNamespace, &cm)
	if err != nil {
		return nil, fmt.Errorf("failed to read ConfigMap %s/%s: %w", cmNamespace, cmName, err)
	}
	out := make(map[string]map[string]string)
	for acc, accInfoStr := range cm.Data {
		accInfoMap := make(map[string]string)
		if err := json.Unmarshal([]byte(accInfoStr), &accInfoMap); err != nil {
			return nil, fmt.Errorf("failed to read entry %s in ConfigMap %s/%s: %w", acc, cmNamespace, cmName, err)
		}
		out[acc] = accInfoMap
	}
	return out, nil
}

func (r *VariantAutoscalingReconciler) getPrometheusConfig(ctx context.Context) (*interfaces.PrometheusConfig, error) {
	// Try environment variables first
	config, err := r.getPrometheusConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to get config from environment: %w", err)
	}
	if config != nil {
		return config, nil
	}

	// Try ConfigMap second
	config, err = r.getPrometheusConfigFromConfigMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get config from ConfigMap: %w", err)
	}
	if config != nil {
		return config, nil
	}

	// No configuration found
	logger.Log.Warn("No Prometheus configuration found. Please set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
	return nil, fmt.Errorf("no Prometheus configuration found. Please set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
}

func (r *VariantAutoscalingReconciler) getPrometheusConfigFromEnv() (*interfaces.PrometheusConfig, error) {
	promAddr := os.Getenv("PROMETHEUS_BASE_URL")
	if promAddr == "" {
		return nil, nil // No config found, but not an error
	}

	logger.Log.Info("Using Prometheus configuration from environment variables", "address", promAddr)
	return utils.ParsePrometheusConfigFromEnv(), nil
}

func (r *VariantAutoscalingReconciler) getPrometheusConfigFromConfigMap(ctx context.Context) (*interfaces.PrometheusConfig, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, configMapName, configMapNamespace, &cm)
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap for Prometheus config: %w", err)
	}

	promAddr, exists := cm.Data["PROMETHEUS_BASE_URL"]
	if !exists || promAddr == "" {
		return nil, nil // No config found, but not an error
	}

	logger.Log.Info("Using Prometheus configuration from ConfigMap", "address", promAddr)

	// Create config from ConfigMap data
	config := &interfaces.PrometheusConfig{
		BaseURL: promAddr,
	}

	// Parse TLS configuration from ConfigMap (TLS is always enabled for HTTPS-only support)
	config.InsecureSkipVerify = utils.GetConfigValue(cm.Data, "PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "") == "true"
	config.CACertPath = utils.GetConfigValue(cm.Data, "PROMETHEUS_CA_CERT_PATH", "")
	config.ClientCertPath = utils.GetConfigValue(cm.Data, "PROMETHEUS_CLIENT_CERT_PATH", "")
	config.ClientKeyPath = utils.GetConfigValue(cm.Data, "PROMETHEUS_CLIENT_KEY_PATH", "")
	config.ServerName = utils.GetConfigValue(cm.Data, "PROMETHEUS_SERVER_NAME", "")

	// Add bearer token if provided
	if bearerToken, exists := cm.Data["PROMETHEUS_BEARER_TOKEN"]; exists && bearerToken != "" {
		config.BearerToken = bearerToken
	}

	return config, nil
}

func (r *VariantAutoscalingReconciler) readOptimizationConfig(ctx context.Context) (interval string, err error) {
	cm := corev1.ConfigMap{}
	err = utils.GetConfigMapWithBackoff(ctx, r.Client, configMapName, configMapNamespace, &cm)

	if err != nil {
		return "", fmt.Errorf("failed to get optimization configmap after retries: %w", err)
	}

	interval = cm.Data["GLOBAL_OPT_INTERVAL"]
	return interval, nil
}

// readScaleToZeroConfig reads per-model scale-to-zero configuration from a ConfigMap
// using prefixed-key format with YAML values.
//
// Format: Keys prefixed with "model.", values are YAML
//
// Example:
//
//	model.meta.llama-3.1-8b: |
//	  modelID: "meta/llama-3.1-8b"
//	  enableScaleToZero: true
//	  retentionPeriod: "5m"
//	__defaults__: |
//	  enableScaleToZero: true
//	  retentionPeriod: "15m"
//
// Benefits:
//   - Independently editable (kubectl patch single model)
//   - Original modelID preserved in YAML value (no collision risk)
//   - Better Git diffs (only changed models shown)
//   - Human-readable YAML format
//
// The function returns an empty map if the ConfigMap is not found (it's optional).
func (r *VariantAutoscalingReconciler) readScaleToZeroConfig(ctx context.Context, cmName, cmNamespace string) (utils.ScaleToZeroConfigData, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, cmName, cmNamespace, &cm)
	if err != nil {
		// ConfigMap is optional - return empty map if not found
		logger.Log.Debug("Scale-to-zero ConfigMap not found, using global defaults", "configMap", cmName, "namespace", cmNamespace)
		return make(utils.ScaleToZeroConfigData), nil
	}

	out := make(utils.ScaleToZeroConfigData)
	// Track which keys define which modelIDs to detect duplicates
	modelIDToKeys := make(map[string][]string)

	logger.Log.Debug("Loading scale-to-zero config from prefixed-key format",
		"configMap", cmName,
		"namespace", cmNamespace)

	// Sort keys to ensure deterministic processing order
	// This is critical because map iteration in Go is non-deterministic.
	// If there are duplicate modelIDs, the lexicographically first key will win.
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		configStr := cm.Data[key]
		// Handle global defaults (special key)
		if key == utils.GlobalDefaultsKey {
			var config utils.ModelScaleToZeroConfig
			if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
				logger.Log.Warn("Failed to parse __defaults__ in scale-to-zero ConfigMap, skipping",
					"configMap", cmName,
					"error", err)
				continue
			}
			out[utils.GlobalDefaultsKey] = config
			continue
		}

		// Handle prefixed model keys
		if strings.HasPrefix(key, "model.") {
			var config utils.ModelScaleToZeroConfig
			if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
				logger.Log.Warn("Failed to parse scale-to-zero config for prefixed key, skipping",
					"key", key,
					"configMap", cmName,
					"error", err)
				continue
			}

			// Use modelID from YAML (not the key) to avoid collision
			if config.ModelID == "" {
				logger.Log.Warn("Skipping model config without modelID field in scale-to-zero ConfigMap",
					"key", key,
					"configMap", cmName)
				continue
			}

			// Check for duplicate modelID
			if existingKeys, exists := modelIDToKeys[config.ModelID]; exists {
				logger.Log.Warn("Duplicate modelID found in scale-to-zero ConfigMap - first key wins (lexicographic order)",
					"modelID", config.ModelID,
					"winningKey", existingKeys[0],
					"duplicateKey", key,
					"configMap", cmName)
				// Skip this duplicate - first key already processed wins
				continue
			}
			modelIDToKeys[config.ModelID] = append(modelIDToKeys[config.ModelID], key)

			out[config.ModelID] = config
		}
	}

	logger.Log.Debug("Loaded scale-to-zero config",
		"configMap", cmName,
		"namespace", cmNamespace,
		"modelCount", len(out))
	return out, nil
}
