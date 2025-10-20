# API Reference

## Packages
- [llmd.ai/v1alpha1](#llmdaiv1alpha1)


## llmd.ai/v1alpha1

Package v1alpha1 contains API Schema definitions for the llmd v1alpha1 API group.

### Resource Types
- [VariantAutoscaling](#variantautoscaling)
- [VariantAutoscalingList](#variantautoscalinglist)



#### AcceleratorProfile



AcceleratorProfile defines the configuration for an accelerator used in autoscaling.
It specifies the type and count of accelerator, as well as parameters for scaling behavior.



_Appears in:_
- [ModelProfile](#modelprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `acc` _string_ | Acc specifies the type or name of the accelerator (e.g., GPU type). |  | MinLength: 1 <br /> |
| `accCount` _integer_ | AccCount specifies the number of accelerator units to be used. |  | Minimum: 1 <br /> |
| `perfParms` _[PerfParms](#perfparms)_ | PerParms specifies the prefill and decode parameters for ttft and itl models |  |  |
| `maxBatchSize` _integer_ | MaxBatchSize is the maximum batch size supported by the accelerator. |  | Minimum: 1 <br /> |


#### ActuationStatus



ActuationStatus provides details about the actuation process and its current status.



_Appears in:_
- [VariantAutoscalingStatus](#variantautoscalingstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `applied` _boolean_ | Applied indicates whether the actuation was successfully applied. |  |  |


#### Allocation



Allocation describes the current resource allocation for a model variant.



_Appears in:_
- [VariantAutoscalingStatus](#variantautoscalingstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `accelerator` _string_ | Accelerator is the type of accelerator currently allocated. |  | MinLength: 1 <br /> |
| `numReplicas` _integer_ | NumReplicas is the number of replicas currently allocated. |  | Minimum: 0 <br /> |
| `maxBatch` _integer_ | MaxBatch is the maximum batch size currently allocated. |  | Minimum: 0 <br /> |
| `variantCost` _string_ | VariantCost is the cost associated with the current variant allocation. |  | Pattern: `^\d+(\.\d+)?$` <br /> |
| `itlAverage` _string_ | ITLAverage is the average inter token latency for the current allocation. |  | Pattern: `^\d+(\.\d+)?$` <br /> |
| `ttftAverage` _string_ | TTFTAverage is the average time to first token for the current allocation |  | Pattern: `^\d+(\.\d+)?$` <br /> |
| `load` _[LoadProfile](#loadprofile)_ | Load describes the workload characteristics for the current allocation. |  |  |


#### ConfigMapKeyRef



ConfigMapKeyRef references a specific key within a ConfigMap.



_Appears in:_
- [VariantAutoscalingSpec](#variantautoscalingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the name of the ConfigMap. |  | MinLength: 1 <br /> |
| `key` _string_ | Key is the key within the ConfigMap. |  | MinLength: 1 <br /> |


#### LoadProfile



LoadProfile represents the configuration for workload characteristics,
including the rate of incoming requests (ArrivalRate) and the average
length of each request (AvgLength). Both fields are specified as strings
to allow flexible input formats.



_Appears in:_
- [Allocation](#allocation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `arrivalRate` _string_ | ArrivalRate is the rate of incoming requests in inference server. |  |  |
| `avgInputTokens` _string_ | AvgInputTokens is the average number of input(prefill) tokens per request in inference server. |  |  |
| `avgOutputTokens` _string_ | AvgOutputTokens is the average number of output(decode) tokens per request in inference server. |  |  |


#### ModelProfile



ModelProfile provides resource and performance characteristics for the model variant.



_Appears in:_
- [VariantAutoscalingSpec](#variantautoscalingspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `accelerators` _[AcceleratorProfile](#acceleratorprofile) array_ | Accelerators is a list of accelerator profiles for the model variant. |  | MinItems: 1 <br /> |


#### OptimizedAlloc



OptimizedAlloc describes the target optimized allocation for a model variant.



_Appears in:_
- [VariantAutoscalingStatus](#variantautoscalingstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `lastRunTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#time-v1-meta)_ | LastRunTime is the timestamp of the last optimization run. |  |  |
| `accelerator` _string_ | Accelerator is the type of accelerator for the optimized allocation. |  | MinLength: 2 <br /> |
| `numReplicas` _integer_ | NumReplicas is the number of replicas for the optimized allocation. |  | Minimum: 0 <br /> |


#### PerfParms







_Appears in:_
- [AcceleratorProfile](#acceleratorprofile)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `decodeParms` _object (keys:string, values:string)_ | DecodeParms contains parameters for the decode phase (ITL calculation)<br />Expected keys: "alpha", "beta" for equation: itl = alpha + beta * maxBatchSize |  | MinProperties: 1 <br /> |
| `prefillParms` _object (keys:string, values:string)_ | PrefillParms contains parameters for the prefill phase (TTFT calculation)<br />Expected keys: "gamma", "delta" for equation: ttft = gamma + delta * tokens * maxBatchSize |  | MinProperties: 1 <br /> |


#### VariantAutoscaling



VariantAutoscaling is the Schema for the variantautoscalings API.
It represents the autoscaling configuration and status for a model variant.



_Appears in:_
- [VariantAutoscalingList](#variantautoscalinglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `llmd.ai/v1alpha1` | | |
| `kind` _string_ | `VariantAutoscaling` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[VariantAutoscalingSpec](#variantautoscalingspec)_ | Spec defines the desired state for autoscaling the model variant. |  |  |
| `status` _[VariantAutoscalingStatus](#variantautoscalingstatus)_ | Status represents the current status of autoscaling for the model variant. |  |  |


#### VariantAutoscalingList



VariantAutoscalingList contains a list of VariantAutoscaling resources.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `llmd.ai/v1alpha1` | | |
| `kind` _string_ | `VariantAutoscalingList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[VariantAutoscaling](#variantautoscaling) array_ | Items is the list of VariantAutoscaling resources. |  |  |


#### VariantAutoscalingSpec



VariantAutoscalingSpec defines the desired state for autoscaling a model variant.



_Appears in:_
- [VariantAutoscaling](#variantautoscaling)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `modelID` _string_ | ModelID specifies the unique identifier of the model to be autoscaled. |  | MinLength: 1 <br />Required: \{\} <br /> |
| `sloClassRef` _[ConfigMapKeyRef](#configmapkeyref)_ | SLOClassRef references the ConfigMap key containing Service Level Objective (SLO) configuration. |  | Required: \{\} <br /> |
| `modelProfile` _[ModelProfile](#modelprofile)_ | ModelProfile provides resource and performance characteristics for the model variant. |  | Required: \{\} <br /> |
| `enableScaleToZero` _boolean_ | EnableScaleToZero enables scaling the model variant to zero replicas when there is no traffic.<br />When enabled, the autoscaler can scale down to 0 replicas during periods of inactivity.<br />If not specified, defaults to the global WVA_SCALE_TO_ZERO setting. |  |  |
| `scaleToZeroPodRetentionPeriod` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#duration-v1-meta)_ | ScaleToZeroPodRetentionPeriod specifies how long to wait after the last request<br />before scaling down to zero replicas. This grace period helps avoid rapid scale-up/scale-down<br />cycles for intermittent traffic patterns.<br />The value must be a valid duration string (e.g., "5m", "1h", "30s").<br />If not specified when EnableScaleToZero is true, defaults to immediate scale-down.<br />This field is ignored when EnableScaleToZero is false. |  | Pattern: `^([0-9]+(\.[0-9]+)?(ns\|us\|µs\|ms\|s\|m\|h))+$` <br />Type: string <br /> |


#### VariantAutoscalingStatus



VariantAutoscalingStatus represents the current status of autoscaling for a variant,
including the current allocation, desired optimized allocation, and actuation status.



_Appears in:_
- [VariantAutoscaling](#variantautoscaling)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `currentAlloc` _[Allocation](#allocation)_ | CurrentAlloc specifies the current resource allocation for the variant. |  |  |
| `desiredOptimizedAlloc` _[OptimizedAlloc](#optimizedalloc)_ | DesiredOptimizedAlloc indicates the target optimized allocation based on autoscaling logic. |  |  |
| `actuation` _[ActuationStatus](#actuationstatus)_ | Actuation provides details about the actuation process and its current status. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.32/#condition-v1-meta) array_ | Conditions represent the latest available observations of the VariantAutoscaling's state |  |  |


