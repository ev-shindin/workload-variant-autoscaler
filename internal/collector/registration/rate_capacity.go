// Package registration provides query registration for metrics sources.
// This file holds the per-pod rate queries shared by the V2 saturation analyzer's
// rate-anchored capacity estimator and the throughput analyzer.
package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// RegisterRateCapacityQueries registers the per-pod rate queries the V2 saturation
// analyzer needs for rate-anchored capacity: the request completion rate (mu) and
// the instantaneous KV utilization.
//
// Both were originally registered by RegisterThroughputAnalyzerQueries, which runs
// only when the throughput analyzer is enabled. The rate-anchored capacity estimator
// must not depend on that analyzer, so the definitions live here and both registrars
// use the register-if-absent helpers below. Either may run first, and running both
// is a no-op for the shared queries rather than a duplicate-registration panic.
func RegisterRateCapacityQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping rate capacity query registration")
		return
	}
	registry := metricsSource.QueryList()

	registerIfAbsent(registry, kvUsageInstantQuery())
	registerIfAbsent(registry, requestRateQuery())
	registerForEngineIfAbsent(registry, inferenceengine.EngineSGLang, sglangKvUsageInstantQuery())
	registerForEngineIfAbsent(registry, inferenceengine.EngineSGLang, sglangRequestRateQuery())
}

// registerIfAbsent registers tmpl unless a query of that name already exists.
// Unlike MustRegister it does not panic on a second registration, which is what
// allows two independent registrars to declare the same shared query.
func registerIfAbsent(registry *source.QueryList, tmpl source.QueryTemplate) {
	if registry.Get(tmpl.Name) != nil {
		return
	}
	registry.MustRegister(tmpl)
}

// registerForEngineIfAbsent is registerIfAbsent for an engine-scoped query name.
// engine is a parameter rather than a constant so a second engine can be added
// without reshaping the call sites, matching registerForEngine.
func registerForEngineIfAbsent(registry *source.QueryList, engine inferenceengine.Engine, tmpl source.QueryTemplate) { //nolint:unparam // mirrors registerForEngine; more engines are expected
	tmpl.Name = EngineQuery(engine, tmpl.Name)
	registerIfAbsent(registry, tmpl)
}

// kvUsageInstantQuery is the per-pod instantaneous KV cache utilization (0.0–1.0).
//
// Deliberately NOT max_over_time: the saturation analyzer's demand path wants the
// 1-minute peak (erring high is the safe direction for demand), while a capacity
// estimate anchored on a peak over-states what the replica can hold. Both readings
// of the same underlying metric are needed, for opposite purposes.
func kvUsageInstantQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (vllm:kv_cache_usage_perc{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0); operating point for capacity estimation",
	}
}

// requestRateQuery is the per-pod request completion rate (req/s), derived from the
// generation-tokens histogram _count, which increments once per completed request.
//
// While a replica has a backlog its completion rate is its service rate, whichever
// resource binds — that is what makes it usable as mu for capacity estimation.
func requestRateQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(vllm:request_generation_tokens_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Request completion rate per pod (req/s); service rate under backlog, fallback for λ_dec without EPP",
	}
}

// sglangKvUsageInstantQuery is the SGLang form of kvUsageInstantQuery.
func sglangKvUsageInstantQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryKvUsageInstant,
		Type:        source.QueryTypePromQL,
		Template:    `max by (instance, pod, llm_d_ai_variant) (sglang:token_usage{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Instantaneous KV cache utilization per pod (0.0–1.0); operating point for capacity estimation (SGLang)",
	}
}

// sglangRequestRateQuery is the SGLang form of requestRateQuery.
func sglangRequestRateQuery() source.QueryTemplate {
	return source.QueryTemplate{
		Name:        QueryRequestRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (instance, pod, llm_d_ai_variant) (rate(sglang:generation_tokens_histogram_count{namespace="{{.namespace}}",model_name="{{.modelID}}"}[1m]))`,
		Params:      []string{source.ParamNamespace, source.ParamModelID},
		Description: "Request completion rate per pod (req/s); service rate under backlog, fallback for λ_dec without EPP (SGLang)",
	}
}
