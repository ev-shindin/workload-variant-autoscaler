package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VariantAutoscalingSpec defines the desired state for autoscaling a model variant.
type VariantAutoscalingSpec struct {
	// ModelID specifies the unique identifier of the model to be autoscaled.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Required
	ModelID string `json:"modelID"`

	// VariantID uniquely identifies this variant (model + accelerator + acceleratorCount combination).
	// This is a business identifier that may contain slashes, dots, and mixed case.
	// Format: {modelID}-{accelerator}-{acceleratorCount}
	// Example: "meta/llama-3.1-8b-A100-4"
	//
	// Note: VariantID (variant_id) is distinct from the VariantAutoscaling resource name (variant_name):
	//   - variant_id (this field): Business identifier, may contain non-K8s-compliant characters
	//   - variant_name (resource.Name): Kubernetes resource name (DNS-1123 compliant)
	//
	// Both identifiers are exposed as Prometheus labels for flexible querying:
	//   - Use variant_name to query by Kubernetes resource (typically matches Deployment name)
	//   - Use variant_id to query by business identifier (model/variant naming)
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^.+-[A-Za-z0-9]+-[1-9][0-9]*$`
	VariantID string `json:"variantID"`

	// Accelerator specifies the accelerator type for this variant (e.g., "A100", "L40S").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Required
	Accelerator string `json:"accelerator"`

	// AcceleratorCount specifies the number of accelerator units per replica.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	AcceleratorCount int `json:"acceleratorCount"`

	// SLOClassRef references the ConfigMap key containing Service Level Objective (SLO) configuration.
	// +kubebuilder:validation:Required
	SLOClassRef ConfigMapKeyRef `json:"sloClassRef"`

	// VariantProfile provides performance characteristics for this variant.
	// +kubebuilder:validation:Required
	VariantProfile VariantProfile `json:"variantProfile"`
}

// ConfigMapKeyRef references a specific key within a ConfigMap.
type ConfigMapKeyRef struct {
	// Name is the name of the ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key within the ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// VariantProfile provides performance characteristics for a specific variant.
type VariantProfile struct {
	// PerfParms specifies the prefill and decode parameters for TTFT and ITL models.
	// +kubebuilder:validation:Required
	PerfParms PerfParms `json:"perfParms"`

	// MaxBatchSize is the maximum batch size supported by this variant.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	MaxBatchSize int `json:"maxBatchSize"`
}

// PerfParms contains performance parameters for the variant.
type PerfParms struct {
	// DecodeParms contains parameters for the decode phase (ITL calculation).
	// Expected keys: "alpha", "beta" for equation: itl = alpha + beta * maxBatchSize
	// +kubebuilder:validation:MinProperties=1
	DecodeParms map[string]string `json:"decodeParms"`

	// PrefillParms contains parameters for the prefill phase (TTFT calculation).
	// Expected keys: "gamma", "delta" for equation: ttft = gamma + delta * tokens * maxBatchSize
	// +kubebuilder:validation:MinProperties=1
	PrefillParms map[string]string `json:"prefillParms"`
}

// VariantAutoscalingStatus represents the current status of autoscaling for a variant,
// including aggregate load metrics, current allocations per variant, desired optimized allocations, and actuation status.
type VariantAutoscalingStatus struct {
	// Load describes the aggregate workload characteristics across all variants for this model.
	// Metrics are collected and aggregated from Prometheus across all deployments serving this modelID.
	Load LoadProfile `json:"load,omitempty"`

	// ITLAverage is the average inter-token latency aggregated across all variants.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	ITLAverage string `json:"itlAverage,omitempty"`

	// TTFTAverage is the average time to first token aggregated across all variants.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	TTFTAverage string `json:"ttftAverage,omitempty"`

	// PrimaryReplicas is a convenience field tracking replicas for the primary allocation.
	// This field enables safe kubectl output and prevents array index errors.
	// It mirrors CurrentAllocs[0].NumReplicas when available.
	// +optional
	PrimaryReplicas int `json:"primaryReplicas,omitempty"`

	// CurrentAllocs specifies the current resource allocations for each variant (one per accelerator type).
	// +optional
	CurrentAllocs []Allocation `json:"currentAllocs,omitempty"`

	// DesiredOptimizedAllocs indicates the target optimized allocations based on autoscaling logic.
	// +optional
	DesiredOptimizedAllocs []OptimizedAlloc `json:"desiredOptimizedAllocs,omitempty"`

	// Actuation provides details about the actuation process and its current status.
	Actuation ActuationStatus `json:"actuation,omitempty"`

	// Conditions represent the latest available observations of the VariantAutoscaling's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Allocation describes the current resource allocation for a specific model variant.
// Each allocation represents a single deployment with a specific accelerator type.
// Aggregate metrics (Load, ITL, TTFT) are stored in VariantAutoscalingStatus.
type Allocation struct {
	// VariantID identifies which variant this allocation belongs to.
	// Format: {modelID}-{accelerator}-{acceleratorCount}
	// +kubebuilder:validation:MinLength=1
	VariantID string `json:"variantID"`

	// Accelerator is the type of accelerator currently allocated.
	// +kubebuilder:validation:MinLength=1
	Accelerator string `json:"accelerator"`

	// NumReplicas is the number of replicas currently allocated.
	// +kubebuilder:validation:Minimum=0
	NumReplicas int `json:"numReplicas"`

	// MaxBatch is the maximum batch size currently allocated.
	// +kubebuilder:validation:Minimum=0
	MaxBatch int `json:"maxBatch"`

	// VariantCost is the cost associated with this specific variant allocation.
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	VariantCost string `json:"variantCost"`
}

// LoadProfile represents the configuration for workload characteristics,
// including the rate of incoming requests (ArrivalRate) and the average
// length of each request (AvgLength). Both fields are specified as strings
// to allow flexible input formats.
type LoadProfile struct {
	// ArrivalRate is the rate of incoming requests in inference server.
	ArrivalRate string `json:"arrivalRate"`

	// AvgInputTokens is the average number of input(prefill) tokens per request in inference server.
	AvgInputTokens string `json:"avgInputTokens"`

	// AvgOutputTokens is the average number of output(decode) tokens per request in inference server.
	AvgOutputTokens string `json:"avgOutputTokens"`
}

// OptimizedAlloc describes the target optimized allocation for a model variant.
type OptimizedAlloc struct {
	// LastRunTime is the timestamp of the last optimization run.
	LastRunTime metav1.Time `json:"lastRunTime,omitempty"`

	// VariantID identifies which variant this optimized allocation belongs to.
	// Format: {modelID}-{accelerator}-{acceleratorCount}
	// +kubebuilder:validation:MinLength=1
	VariantID string `json:"variantID"`

	// Accelerator is the type of accelerator for the optimized allocation.
	// +kubebuilder:validation:MinLength=1
	Accelerator string `json:"accelerator"`

	// NumReplicas is the number of replicas for the optimized allocation.
	// +kubebuilder:validation:Minimum=0
	NumReplicas int `json:"numReplicas"`
}

// ActuationStatus provides details about the actuation process and its current status.
type ActuationStatus struct {
	// Applied indicates whether the actuation was successfully applied.
	Applied bool `json:"applied"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=va
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=".spec.modelID"
// +kubebuilder:printcolumn:name="Variant",type=string,JSONPath=".spec.variantID"
// +kubebuilder:printcolumn:name="Accelerator",type=string,JSONPath=".spec.accelerator"
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=".status.primaryReplicas"
// +kubebuilder:printcolumn:name="ArrivalRate",type=string,JSONPath=".status.load.arrivalRate"
// +kubebuilder:printcolumn:name="MetricsReady",type=string,JSONPath=".status.conditions[?(@.type=='MetricsAvailable')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// VariantAutoscaling is the Schema for the variantautoscalings API.
// It represents the autoscaling configuration and status for a model variant.
type VariantAutoscaling struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state for autoscaling the model variant.
	Spec VariantAutoscalingSpec `json:"spec,omitempty"`

	// Status represents the current status of autoscaling for the model variant.
	Status VariantAutoscalingStatus `json:"status,omitempty"`
}

// VariantAutoscalingList contains a list of VariantAutoscaling resources.
// +kubebuilder:object:root=true
type VariantAutoscalingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// Items is the list of VariantAutoscaling resources.
	Items []VariantAutoscaling `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VariantAutoscaling{}, &VariantAutoscalingList{})
}

// Condition Types for VariantAutoscaling
const (
	// TypeMetricsAvailable indicates whether vLLM metrics are available from Prometheus
	TypeMetricsAvailable = "MetricsAvailable"
	// TypeOptimizationReady indicates whether the optimization engine can run successfully
	TypeOptimizationReady = "OptimizationReady"
)

// Condition Reasons for MetricsAvailable
const (
	// ReasonMetricsFound indicates vLLM metrics were successfully retrieved
	ReasonMetricsFound = "MetricsFound"
	// ReasonMetricsMissing indicates vLLM metrics are not available (likely ServiceMonitor issue)
	ReasonMetricsMissing = "MetricsMissing"
	// ReasonMetricsStale indicates metrics exist but are outdated
	ReasonMetricsStale = "MetricsStale"
	// ReasonPrometheusError indicates error querying Prometheus
	ReasonPrometheusError = "PrometheusError"
)

// Condition Reasons for OptimizationReady
const (
	// ReasonOptimizationSucceeded indicates optimization completed successfully
	ReasonOptimizationSucceeded = "OptimizationSucceeded"
	// ReasonOptimizationFailed indicates optimization failed
	ReasonOptimizationFailed = "OptimizationFailed"
	// ReasonMetricsUnavailable indicates optimization cannot run due to missing metrics
	ReasonMetricsUnavailable = "MetricsUnavailable"
)
