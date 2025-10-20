package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
)

// Global backoff configurations
var (
	// Standard backoff for most operations
	StandardBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    5,
	}

	// Slow backoff for operations that need more time
	ReconcileBackoff = wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   2.0,
		Steps:    5,
	}

	// Prometheus validation backoff with longer intervals
	// TODO: investigate why Prometheus needs longer backoff durations
	PrometheusBackoff = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    6, // 5s, 10s, 20s, 40s, 80s, 160s = ~5 minutes total
	}
)

// IsScaleToZeroEnabled determines if scale-to-zero is enabled for a specific VariantAutoscaling.
// It checks the per-model EnableScaleToZero field first, and falls back to the global
// WVA_SCALE_TO_ZERO environment variable if the per-model setting is not specified.
// Returns true if scale-to-zero should be enabled for this variant.
func IsScaleToZeroEnabled(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) bool {
	// Check per-model setting first
	if va.Spec.EnableScaleToZero != nil {
		return *va.Spec.EnableScaleToZero
	}

	// Fall back to global environment variable
	return strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true")
}

// GetScaleToZeroRetentionPeriod returns the pod retention period for scale-to-zero.
// If the per-model ScaleToZeroPodRetentionPeriod is specified, it returns that duration.
// Otherwise, it returns 0 (immediate scale-down).
// This period defines how long to wait after the last request before scaling to zero.
func GetScaleToZeroRetentionPeriod(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) time.Duration {
	if va.Spec.ScaleToZeroPodRetentionPeriod != nil {
		return va.Spec.ScaleToZeroPodRetentionPeriod.Duration
	}
	return 0
}

// GetMinNumReplicas returns the minimum number of replicas based on scale-to-zero configuration.
// Returns 0 if scale-to-zero is enabled, otherwise returns 1.
func GetMinNumReplicas(va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) int {
	if IsScaleToZeroEnabled(va) {
		return 0
	}
	return 1
}

// GetResourceWithBackoff performs a Get operation with exponential backoff retry logic
func GetResourceWithBackoff[T client.Object](ctx context.Context, c client.Client, objKey client.ObjectKey, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, objKey, obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, err // Don't retry on notFound errors
			}

			logger.Log.Error(err, "transient error getting resource, retrying - ",
				"resourceType: ", resourceType,
				" name: ", objKey.Name,
				" namespace: ", objKey.Namespace)
			return false, nil // Retry on transient errors
		}

		return true, nil
	})
}

// Helper functions for common resource types with standard backoff
func GetDeploymentWithBackoff(ctx context.Context, c client.Client, name, namespace string, deploy *appsv1.Deployment) error {
	return GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, deploy, StandardBackoff, "Deployment")
}

func GetConfigMapWithBackoff(ctx context.Context, c client.Client, name, namespace string, cm *corev1.ConfigMap) error {
	return GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, cm, StandardBackoff, "ConfigMap")
}

func GetVariantAutoscalingWithBackoff(ctx context.Context, c client.Client, name, namespace string, va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling) error {
	return GetResourceWithBackoff(ctx, c, client.ObjectKey{Name: name, Namespace: namespace}, va, StandardBackoff, "VariantAutoscaling")
}

// UpdateStatusWithBackoff performs a Status Update operation with exponential backoff retry logic
func UpdateStatusWithBackoff[T client.Object](ctx context.Context, c client.Client, obj T, backoff wait.Backoff, resourceType string) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		err := c.Status().Update(ctx, obj)
		if err != nil {
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
				logger.Log.Error(err, "permanent error updating status for resource ", resourceType, ", name: ", obj.GetName())
				return false, err // Don't retry on permanent errors
			}
			logger.Log.Error(err, "transient error updating status, retrying for resource ", resourceType, ", name: ", obj.GetName())
			return false, nil // Retry on transient errors
		}
		return true, nil
	})
}

// Adapter to create wva system data types from config maps.
// Note: WVA operates in unlimited mode, so capacity data is not used.
func CreateSystemData(
	acceleratorCm map[string]map[string]string,
	serviceClassCm map[string]string) *infernoConfig.SystemData {

	systemData := &infernoConfig.SystemData{
		Spec: infernoConfig.SystemSpec{
			Accelerators:   infernoConfig.AcceleratorData{},
			Models:         infernoConfig.ModelData{},
			ServiceClasses: infernoConfig.ServiceClassData{},
			Servers:        infernoConfig.ServerData{},
			Optimizer:      infernoConfig.OptimizerData{},
			Capacity:       infernoConfig.CapacityData{},
		},
	}

	// get accelerator data
	acceleratorData := []infernoConfig.AcceleratorSpec{}
	for key, val := range acceleratorCm {
		cost, err := strconv.ParseFloat(val["cost"], 32)
		if err != nil {
			logger.Log.Warn("failed to parse accelerator cost in configmap, skipping accelerator", "name", key)
			continue
		}
		acceleratorData = append(acceleratorData, infernoConfig.AcceleratorSpec{
			Name:         key,
			Type:         val["device"],
			Multiplicity: 1,                         // TODO: multiplicity should be in the configured accelerator spec
			Power:        infernoConfig.PowerSpec{}, // Not currently used
			Cost:         float32(cost),
		})
	}
	systemData.Spec.Accelerators.Spec = acceleratorData

	// Capacity data is not used in unlimited mode - initialize empty for future limited mode work
	systemData.Spec.Capacity.Count = []infernoConfig.AcceleratorCount{}

	// get service class data
	serviceClassData := []infernoConfig.ServiceClassSpec{}
	for key, val := range serviceClassCm {
		var sc interfaces.ServiceClass
		if err := yaml.Unmarshal([]byte(val), &sc); err != nil {
			logger.Log.Warn("failed to parse service class data, skipping service class", "key", key, "err", err)
			continue
		}
		serviceClassSpec := infernoConfig.ServiceClassSpec{
			Name:         sc.Name,
			Priority:     sc.Priority,
			ModelTargets: make([]infernoConfig.ModelTarget, len(sc.Data)),
		}
		for i, entry := range sc.Data {
			serviceClassSpec.ModelTargets[i] = infernoConfig.ModelTarget{
				Model:    entry.Model,
				SLO_ITL:  float32(entry.SLOTPOT),
				SLO_TTFT: float32(entry.SLOTTFT),
			}
		}
		serviceClassData = append(serviceClassData, serviceClassSpec)
	}
	systemData.Spec.ServiceClasses.Spec = serviceClassData

	// set optimizer configuration
	// TODO: make it configurable
	systemData.Spec.Optimizer.Spec = infernoConfig.OptimizerSpec{
		Unlimited: true,
		// SaturationPolicy omitted - defaults to "None" (not relevant in unlimited mode)
	}

	// initialize model data
	systemData.Spec.Models.PerfData = []infernoConfig.ModelAcceleratorPerfData{}

	// initialize dynamic server data
	systemData.Spec.Servers.Spec = []infernoConfig.ServerSpec{}

	return systemData
}

// add model accelerator pair profile data to inferno system data
func AddModelAcceleratorProfileToSystemData(
	sd *infernoConfig.SystemData,
	modelName string,
	modelAcceleratorProfile *llmdVariantAutoscalingV1alpha1.AcceleratorProfile) (err error) {

	// extract decode model (itl) parameters
	decodeParms := modelAcceleratorProfile.PerfParms.DecodeParms
	if len(decodeParms) < 2 {
		return fmt.Errorf("length of decodeParms should be 2")
	}

	var alpha, beta float64
	if alpha, err = strconv.ParseFloat(decodeParms["alpha"], 32); err != nil {
		return err
	}
	if beta, err = strconv.ParseFloat(decodeParms["beta"], 32); err != nil {
		return err
	}

	// extract prefill model (ttft) parameters
	prefillParms := modelAcceleratorProfile.PerfParms.PrefillParms
	if len(prefillParms) < 2 {
		return fmt.Errorf("length of prefillParms should be 2")
	}

	var gamma, delta float64
	if gamma, err = strconv.ParseFloat(prefillParms["gamma"], 32); err != nil {
		return err
	}
	if delta, err = strconv.ParseFloat(prefillParms["delta"], 32); err != nil {
		return err
	}

	sd.Spec.Models.PerfData = append(sd.Spec.Models.PerfData,
		infernoConfig.ModelAcceleratorPerfData{
			Name:         modelName,
			Acc:          modelAcceleratorProfile.Acc,
			AccCount:     modelAcceleratorProfile.AccCount,
			MaxBatchSize: modelAcceleratorProfile.MaxBatchSize,
			DecodeParms: infernoConfig.DecodeParms{
				Alpha: float32(alpha),
				Beta:  float32(beta),
			},
			PrefillParms: infernoConfig.PrefillParms{
				Gamma: float32(gamma),
				Delta: float32(delta),
			},
		})
	return nil
}

// Add server specs to inferno system data
func AddServerInfoToSystemData(
	sd *infernoConfig.SystemData,
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	className string) (err error) {

	// server load statistics
	var arrivalRate, avgOutputTokens, avgInputTokens, cost, itlAverage, ttftAverage float64
	if arrivalRate, err = strconv.ParseFloat(va.Status.CurrentAlloc.Load.ArrivalRate, 32); err != nil || !CheckValue(arrivalRate) {
		arrivalRate = 0
	}
	if avgOutputTokens, err = strconv.ParseFloat(va.Status.CurrentAlloc.Load.AvgOutputTokens, 32); err != nil || !CheckValue(avgOutputTokens) {
		avgOutputTokens = 0
	}
	if avgInputTokens, err = strconv.ParseFloat(va.Status.CurrentAlloc.Load.AvgInputTokens, 32); err != nil || !CheckValue(avgInputTokens) {
		avgInputTokens = 0
	}

	serverLoadSpec := &infernoConfig.ServerLoadSpec{
		ArrivalRate:  float32(arrivalRate),
		AvgInTokens:  int(avgInputTokens),
		AvgOutTokens: int(avgOutputTokens),
	}

	// server allocation
	if cost, err = strconv.ParseFloat(va.Status.CurrentAlloc.VariantCost, 32); err != nil || !CheckValue(cost) {
		cost = 0
	}
	if itlAverage, err = strconv.ParseFloat(va.Status.CurrentAlloc.ITLAverage, 32); err != nil || !CheckValue(itlAverage) {
		itlAverage = 0
	}
	if ttftAverage, err = strconv.ParseFloat(va.Status.CurrentAlloc.TTFTAverage, 32); err != nil || !CheckValue(ttftAverage) {
		ttftAverage = 0
	}

	AllocationData := &infernoConfig.AllocationData{
		Accelerator: va.Status.CurrentAlloc.Accelerator,
		NumReplicas: va.Status.CurrentAlloc.NumReplicas,
		MaxBatch:    va.Status.CurrentAlloc.MaxBatch,
		Cost:        float32(cost),
		ITLAverage:  float32(itlAverage),
		TTFTAverage: float32(ttftAverage),
		Load:        *serverLoadSpec,
	}

	// all server data
	// Determine minimum replicas based on per-model or global scale-to-zero configuration
	minNumReplicas := GetMinNumReplicas(va)

	// Get retention period (for future use in scale-to-zero logic)
	retentionPeriod := GetScaleToZeroRetentionPeriod(va)
	if retentionPeriod > 0 {
		logger.Log.Debug("Scale-to-zero retention period configured",
			"variant", va.Name,
			"namespace", va.Namespace,
			"retentionPeriod", retentionPeriod)
	}

	serverSpec := &infernoConfig.ServerSpec{
		Name:            FullName(va.Name, va.Namespace),
		Class:           className,
		Model:           va.Spec.ModelID,
		KeepAccelerator: true,
		MinNumReplicas:  minNumReplicas,
		CurrentAlloc:    *AllocationData,
		DesiredAlloc:    infernoConfig.AllocationData{},
	}

	// set max batch size if configured
	maxBatchSize := 0
	accName := va.Labels["inference.optimization/acceleratorName"]
	for _, ap := range va.Spec.ModelProfile.Accelerators {
		if ap.Acc == accName {
			maxBatchSize = ap.MaxBatchSize
			break
		}
	}
	if maxBatchSize > 0 {
		serverSpec.MaxBatchSize = maxBatchSize
	}

	sd.Spec.Servers.Spec = append(sd.Spec.Servers.Spec, *serverSpec)
	return nil
}

// Adapter from inferno alloc solution to optimized alloc
func CreateOptimizedAlloc(name string,
	namespace string,
	allocationSolution *infernoConfig.AllocationSolution) (*llmdVariantAutoscalingV1alpha1.OptimizedAlloc, error) {

	serverName := FullName(name, namespace)
	var allocationData infernoConfig.AllocationData
	var exists bool
	if allocationData, exists = allocationSolution.Spec[serverName]; !exists {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	logger.Log.Debug("Setting accelerator name ", "Name ", allocationData.Accelerator, "allocationData ", allocationData)
	optimizedAlloc := &llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
		LastRunTime: metav1.NewTime(time.Now()),
		Accelerator: allocationData.Accelerator,
		NumReplicas: allocationData.NumReplicas,
	}
	return optimizedAlloc, nil
}

// Helper to create a (unique) full name from name and namespace
func FullName(name string, namespace string) string {
	return name + ":" + namespace
}

// Helper to check if a value is valid (not NaN or infinite)
func CheckValue(x float64) bool {
	return !(math.IsNaN(x) || math.IsInf(x, 0))
}

func GetZapLevelFromEnv() zapcore.Level {
	levelStr := strings.ToLower(os.Getenv("LOG_LEVEL"))
	switch levelStr {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel // fallback
	}
}

func MarshalStructToJsonString(t any) string {
	jsonBytes, err := json.MarshalIndent(t, "", " ")
	if err != nil {
		return fmt.Sprintf("error marshalling: %v", err)
	}
	re := regexp.MustCompile("\"|\n")
	return re.ReplaceAllString(string(jsonBytes), "")
}

// Helper to find SLOs for a model variant
func FindModelSLO(cmData map[string]string, targetModel string) (*interfaces.ServiceClassEntry, string /* class name */, error) {
	for key, val := range cmData {
		var sc interfaces.ServiceClass
		if err := yaml.Unmarshal([]byte(val), &sc); err != nil {
			return nil, "", fmt.Errorf("failed to parse %s: %w", key, err)
		}

		for _, entry := range sc.Data {
			if entry.Model == targetModel {
				return &entry, sc.Name, nil
			}
		}
	}
	return nil, "", fmt.Errorf("model %q not found in any service class", targetModel)
}

func Ptr[T any](v T) *T {
	return &v
}

// ValidatePrometheusAPIWithBackoff validates Prometheus API connectivity with retry logic
func ValidatePrometheusAPIWithBackoff(ctx context.Context, promAPI promv1.API, backoff wait.Backoff) error {
	return wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Test with a simple query that should always work
		query := "up"
		_, _, err := promAPI.Query(ctx, query, time.Now())
		if err != nil {
			logger.Log.Error(err, "Prometheus API validation failed, retrying - ",
				"query: ", query,
				"error: ", err.Error())
			return false, nil // Retry on transient errors
		}

		logger.Log.Info("Prometheus API validation successful with query", "query", query)
		return true, nil
	})
}

// ValidatePrometheusAPI validates Prometheus API connectivity using standard Prometheus backoff
func ValidatePrometheusAPI(ctx context.Context, promAPI promv1.API) error {
	return ValidatePrometheusAPIWithBackoff(ctx, promAPI, PrometheusBackoff)
}

// GetConfigValue retrieves a value from a ConfigMap with a default fallback
func GetConfigValue(data map[string]string, key, def string) string {
	if v, ok := data[key]; ok {
		return v
	}
	return def
}
