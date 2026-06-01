# Proposal: Pod → `VariantAutoscaling` Mapping via Informer-Backed Cache

---

## Problem Statement

WVA must associate every vLLM Prometheus metric with the `VariantAutoscaling` that owns the pod producing it. Per-pod metrics (KV-cache usage, queue depth, latencies, cache config) feed per-VA scaling decisions on every reconcile; the mapping sits on the critical path of the autoscaler.

The current implementation, landed by PR #1145 (merged 2026-05-19), is **tenant-driven**. Per `deploy/README.md:68`, to make one `VariantAutoscaling` actually receive metrics, a tenant must author **two coordinated artifacts** beyond the workload itself:

1. `llm-d.ai/variant: <VA-name>` on the pod template's `metadata.labels`. For `LeaderWorkerSet` the label has to be set on *both* `leaderTemplate` and `workerTemplate`. The value must equal the `VariantAutoscaling`'s `metadata.name` exactly — there is no admission check on the link.
2. A `metricRelabelings` rule on the ServiceMonitor/PodMonitor that propagates `__meta_kubernetes_pod_label_llmd_ai_variant → llm_d_ai_variant` into the Prometheus time series.

Each is an independent silent-failure mode. A wrong label key, a value drifted from the VA name, a missing relabeling rule, a half-templated LWS, the wrong monitor scope — any one of them yields zero matching metrics. The collector then falls through to `computeReplicaCapacityFallback` (the same fallback path the `cache_config_info` bug exposed in PR #1198) and the autoscaler produces "working" but quantitatively wrong scaling. The tenant sees no error. None of this gives the tenant anything they actually want — it is coordination overhead to satisfy the collector's identification need.

Issue #1072 names the same fragility but proposes the opposite direction: have the *controller* stamp the label onto pod templates. That trades tenant friction for new costs — rolling restarts when the controller first reconciles a pre-existing workload, GitOps drift, mutation of tenant-owned resources, and kind-specific *write* paths replacing the kind-specific *read* paths #1145 removed. PR #1145 took neither extreme: it kept WVA read-only and asked the tenant to pay the friction.

### Context: PR #1145 and VA CRD deprecation

Three decisions shape this proposal:

- **PR #1145** introduced the required label, added the ServiceMonitor relabeling infrastructure, and **deleted** `internal/collector/source/pod_va_mapper.go` (142 lines + 555-line test). The codebase currently has no read-side derivation of pod→VA; it relies exclusively on the tenant-supplied label.
- **`docs/proposals/deprecate-va-crd.md`** is moving variant *discovery* from the `VariantAutoscaling` CRD onto annotations on `ScaledObject`/`HPA`. After deprecation, the `<VA-name>` anchor that the required label's value tracks no longer has a stable referent.
- **`docs/proposals/design-config-ux.md`** commits to a persona contract — *tenants author their workload + policy; WVA reads, never writes* — that the current required-label-plus-relabel-rule contract partially undermines.

---

## Design Philosophy

WVA derives the pod→VA mapping inside the controller, from data it already watches. The tenant authors nothing WVA-specific in the workload's YAML. The controller mutates nothing the tenant owns. The fragility cited in #1072 (race conditions, label-name inconsistency, silent drops) is real but **implementation-level** — it is solvable inside WVA, not by requiring three coordinated authoring artifacts from the tenant, and not by mutating their `Deployment`/`LWS`.

The cache exposes a single internal API — `Lookup(podKey) → discovery key` — which the rest of the controller depends on. What that key *means* today is a `VariantAutoscaling`; what it means after VA deprecation is "the discovery source for this pod" (an annotated `ScaledObject`/`HPA`). Same API, different value type. The proposal lands a stable internal contract that absorbs the discovery-surface migration without API churn.

---

## Goals

1. Zero new authoring requirement on the tenant for the default pod→VA mapping path.
2. No mutation of tenant-owned resources (`Deployment`, `LWS`, `ServiceMonitor`).
3. Cache misses surface as a counter and a `VariantAutoscaling` status condition — not as silent metric drops that degrade into wrong-capacity fallback downstream.
4. Forward-compatible with the VA CRD deprecation: the cache's value type is the abstract "discovery key", which absorbs the migration from a CRD to annotations without consumer changes.
5. Keep `llm-d.ai/variant` as an *opt-in* tenant-supplied override for topologies the traversal cannot resolve (custom workload kinds, non-standard composition).

## Non-Goals

- Reverting PR #1145 — multi-vLLM-per-pod support (`buildInstanceKey`), the ServiceMonitor relabeling infrastructure, and the test updates stay. This is a corrective follow-up, not a revert (see Alternatives Considered #3).
- Implementing controller-side mutation of pod templates (the direction #1072 proposes). This proposal rejects that direction explicitly.
- Adding admission-time validation that the optional escape-hatch label's value tracks a VA name. The cache is the source of truth; the escape hatch is best-effort.
- Replacing the ServiceMonitor / PodMonitor configuration — the relabeling rule in `config/prometheus/vllm-servicemonitor.yaml` stays for installs that keep the opt-in label.

---

## Proposed Solution

### Informer-Backed Pod→VA Cache

A new `internal/collector/source/pod_va_cache.go` maintains an in-memory map keyed by pod (`namespace/name`, or `namespace/name:port` for multi-vLLM pods) whose value is the discovery key — a `VariantAutoscaling` namespaced key today, an annotated-workload key after VA deprecation. The cache is driven by controller-runtime informers. The `VariantAutoscaling`, `Deployment`, and `LeaderWorkerSet` watches **already exist** in the controller; the **`Pod` watch is new** — WVA watches no pods today (see [Watch & Memory Cost](#watch--memory-cost) for the cost this adds and the scoping that bounds it):

- `VariantAutoscaling` add/update → recompute mappings for its scale target's pods *(existing watch)*.
- `Deployment` / `LeaderWorkerSet` add/update → recompute mappings for their selected pods *(existing watch)*.
- `Pod` add/update → resolve once via owner refs + the indexed VA map; cache the result *(**new** watch)*.
- Delete events → evict.

The 4-hop walk that previously ran *per metric* now runs *once per watch event*, behind the informer. Lookups on the metric hot path are O(1).

### Hot-Path Lookup

`internal/collector/replica_metrics.go`'s per-metric processing becomes a single cache lookup. There are no Kubernetes API calls in the metrics-collection path. The eight repeated `pod`/`pod_name` extraction blocks in the current collector collapse into one `extractPodKey(labels)` helper at the boundary, normalizing the two label-name conventions in a single place.

### Cache Misses as a Signal

When a metric arrives for a pod the cache cannot resolve, WVA increments `wva_pod_va_cache_miss_total{reason}` and sets a `PodMappingMissing` status condition on the relevant `VariantAutoscaling` (post-deprecation: an event on the discovery owner). The dominant cause is the race window between pod creation and informer sync — typically shorter than the first scrape interval — so most misses are transient. The point is that they are now *visible* misses, not silent fallbacks.

### Optional Tenant-Supplied Override

`VariantLabelKey = "llm-d.ai/variant"` stays in `internal/constants/labels.go`. When present on a pod's metric labels, WVA reads it and uses it with *higher precedence* than the traversal result. This covers custom workload kinds and exotic owner chains where the traversal can't infer. WVA never writes the label; the ServiceMonitor relabeling rule continues to propagate it for installs that choose the opt-in path.

### Tenant-Facing UX

Author a `VariantAutoscaling` referencing a `Deployment`/`LWS` — done. The same shape as `HorizontalPodAutoscaler` against a `Deployment`. No required label. No required relabeling rule. No LWS double-template. No Prometheus relabeling knowledge. After VA deprecation, the same applies with discovery moved to annotations on `ScaledObject`/`HPA`.

---

## Watch & Memory Cost

This is the proposal's main resource trade-off and the one point that most needs to be designed in rather than assumed away (raised in review by @dumb0002).

**WVA has no Pod watch today.** Its controllers watch `VariantAutoscaling` (primary), `ServiceMonitor`, `Deployment`, and `LeaderWorkerSet` (`internal/controller/variantautoscaling_controller.go`), plus `ConfigMap`, `HorizontalPodAutoscaler`, `ScaledObject`, and `InferencePool` in their own reconcilers. **None watch `Pod`.** The informer-backed cache therefore introduces **WVA's first always-on Pod informer** — genuinely new API-server load and new cache memory, not a free side-index over watches WVA already runs. (The pod-scraping source lists pods on demand within a single service's namespace; it does not establish a continuous cluster-wide Pod watch.)

**Scope determines cost.** The cache scope follows the manager's, set by `--watch-namespace` (`cmd/main.go`): unset ⇒ all namespaces (cluster-scoped), set ⇒ a single namespace.

| | Namespace-scoped (`--watch-namespace=ns`) | Cluster-scoped (default) |
|---|---|---|
| Pod LIST/WATCH | one namespace | **whole cluster** |
| Event volume | pod churn in that namespace | **all pod churn cluster-wide** — every Deployment/Job/DaemonSet rollout, not just vLLM |
| Cache memory | pods in that namespace | **every `Pod` object in the cluster** |

The dominant cost is **memory**: a controller-runtime informer caches the **full `Pod` object** (spec + status), not the small `podKey → discovery-key` map the cache exposes. An estimate counting only the side-index map ("one entry per managed pod") understates the real footprint by the size of the cached pod objects. On a large multi-tenant cluster (10k+ pods) an unfiltered cluster-scoped Pod cache can reach hundreds of MB to GBs.

**Mitigations — and existing precedent in WVA.** WVA already scopes an informer to cut memory: in multi-namespace mode the ConfigMap cache is filtered by label via `cache.Options.ByObject` (`cmd/main.go`, commented "significantly reduces memory usage"). The Pod informer uses the same mechanism, as a **Phase 1 deliverable, not a later optimization**:

1. **Field/label selector on the Pod cache** — `ByObject[&corev1.Pod{}]{Label/Field, Namespaces}`. Scope to pods of managed scale targets by selecting on labels the **workload already carries** (the `Deployment`/`LWS` pod-template selector WVA reads from the scale target) — *not* a new WVA-specific pod label, which would reintroduce the tenant-labeling dependency this proposal removes. Optionally exclude terminal pods (`status.phase` ∉ {`Succeeded`,`Failed`}).
2. **Restrict watched namespaces to those containing a `VariantAutoscaling`** — the controller already tracks discovery namespaces (the HPA/ScaledObject reconcilers maintain this set), so the cluster-scoped default can be narrowed to active namespaces.
3. **`TransformFunc` on cached Pods** — strip `managedFields`, env, and volume detail before the object enters the cache; the cache only needs owner refs, labels, namespace/name, and ready status.

**Recommendation.** The scoped Pod informer (selector + transform) is required, not optional, and lands in Phase 1. For very large clusters, namespace-scoped deployment is the supported high-scale mode. The memory cost is real and is **bounded by the selector** — the bound must be part of the design.

---

## What Does NOT Change

- The `VariantLabelKey = "llm-d.ai/variant"` constant in `internal/constants/labels.go` (now opt-in, not required).
- The `VariantLabelPrometheusKey = "llm_d_ai_variant"` constant and the `metricRelabelings` rule in `config/prometheus/vllm-servicemonitor.yaml` (used by the opt-in path).
- PromQL query templates' `by (instance, pod, llm_d_ai_variant, …)` grouping clauses — harmless when the variant label is absent, meaningful when the opt-in path is in use.
- vLLM / EPP metric collection, the analyzer pipeline (saturation V1/V2, queueing model, throughput), the optimizer, and the `wva_desired_replicas` output.
- The `VariantAutoscaling` API itself (until VA deprecation lands).

---

## Migration Path

### What Changes for Users

| Before (current, post-#1145) | After |
|---|---|
| Add `llm-d.ai/variant: <VA-name>` to every Deployment pod template | No required label; nothing to author |
| Add the same label to *both* `leaderTemplate` and `workerTemplate` on LWS | Same — no label needed |
| Ensure ServiceMonitor / PodMonitor has the `__meta_kubernetes_pod_label_llmd_ai_variant → llm_d_ai_variant` relabeling rule | Rule still installed; required only if using the opt-in override |
| Silent metric drop → wrong-capacity fallback on misconfiguration | `wva_pod_va_cache_miss_total` + `PodMappingMissing` condition on the VA |
| Cannot use WVA against workloads the tenant cannot label | Default path works for any supported workload kind; opt-in label remains for the long tail |

### Migration Tool

None required. The change is non-breaking: installs that retain `llm-d.ai/variant` continue to work via the opt-in precedence path, and the cache resolves transparently for workloads that drop it.

---

## Implementation Phases

### Phase 1: Informer cache alongside the existing label path (Non-Breaking)

**Goal:** Add the cache; both paths work; the tenant-supplied label takes precedence when present.

**Deliverables:**

- `internal/collector/source/pod_va_cache.go` with informer watches on `VariantAutoscaling`, `Deployment`, `LeaderWorkerSet`, and `Pod`.
- A **scoped Pod informer** — label/field selector + `TransformFunc` configured via `cache.Options.ByObject`, following the existing ConfigMap-cache precedent — so the new Pod watch does not cache every pod in the cluster (see [Watch & Memory Cost](#watch--memory-cost)).
- `Lookup(podKey) → VariantKey` API plus a single `extractPodKey(labels)` helper at the collector boundary, replacing the per-metric `pod`/`pod_name` fallback in `internal/collector/replica_metrics.go`.
- `wva_pod_va_cache_miss_total{reason}` counter and a `PodMappingMissing` status condition on `VariantAutoscaling`.
- Documented opt-in precedence for the `llm-d.ai/variant` label: cache reads it when present, never writes it.

**Success Criteria:** A workload with no `llm-d.ai/variant` label, plus a `VariantAutoscaling` pointing at it, produces metrics matched to the VA without any additional configuration. Cache-miss counter is zero in steady state.

### Phase 2: Documentation flip

**Goal:** Demote the label from "required" to "opt-in escape hatch" in user-facing docs.

**Deliverables:**

- `deploy/README.md` — remove the "WVA does not add this label automatically; you must…" wording and the LWS double-template instructions from the Workload Requirements section. Re-cast the label as an escape hatch with a brief "when you need this" rationale.
- `docs/design/controller-behavior.md` — same demotion in the Prerequisites section #1145 added. Add a troubleshooting entry for `PodMappingMissing`.
- Samples in `config/samples/` and fixtures in `test/e2e/fixtures/` — drop the now-unnecessary label from the default cases; retain one fixture that exercises the opt-in path.

**Success Criteria:** Default samples and e2e tests pass with the label absent from default fixtures.

### Phase 3: Compose with VA Deprecation

**Goal:** Cache values become the abstract "discovery key" (a VA today, an annotated `ScaledObject`/`HPA` post-deprecation).

**Deliverables:**

- Cache value type generalized; lookup callers updated.
- Status-condition / event emission generalized to the new owner kind.
- No tenant-visible change.

**Success Criteria:** Installs running on either the VA-CRD path or the annotation-discovery path resolve pod→discovery-source through the same cache API.

---

## Alternatives Considered

1. **Status quo: tenant-required label + ServiceMonitor relabeling rule (PR #1145 as merged).** Achieves "no API calls per metric" by demanding two coordinated authoring artifacts from the tenant. The silent-fallback failure modes are the primary motivation for this proposal. Strictly worse on adoption friction and observability than C; equal on mutation-of-tenant-resources.

2. **Controller stamps `llm-d.ai/variant` on pod templates (issue #1072's proposal).** Zero tenant authoring burden, at the cost of (a) mutating tenant-owned `Deployment`/`LWS` `spec.template` (rolling restart on first reconcile, perma-drift in GitOps installs), (b) kind-specific *write* paths replacing the kind-specific *read* paths #1145 removed (no net simplification), and (c) direct conflict with the tenant-ownership persona of the config-ux proposal. The "fit with llm-d architecture" framing in #1072 also points the *other way* — the inference scheduler's P/D `role` label is set by the workload author, not by a controller.

3. **Revert PR #1145.** A full revert would also undo multi-vLLM-per-pod support, the `buildInstanceKey` helper, the ServiceMonitor relabeling infrastructure, the test updates, and the doc work — none of which this proposal disagrees with. The deleted `pod_va_mapper.go` was an *unhardened* version of what this proposal builds; recoverable from history if useful but small enough to rewrite. The corrective work is a follow-up PR layered on top of #1145, not a revert.

4. **Validating admission policy on tenant pod templates.** A `ValidatingAdmissionPolicy` (GA in K8s 1.30) could enforce that a managed workload's pod template carries `llm-d.ai/variant`, surfacing typos at admission time. This converts one silent-failure mode into a loud one but leaves the *requirement* itself in place and adds a cluster-level policy surface. Not a substitute for removing the requirement.

5. **External sidecar / DaemonSet that watches pods and stamps the label out-of-tree.** Adds an operational component that does the same thing the controller already could. Rejected on complexity grounds — one less component is the better default.

---

## Comparison Matrix

| Property | A: Status quo (#1145) | B: Controller stamps (#1072) | **C: Informer cache (this proposal)** |
|---|---|---|---|
| New things the tenant must author | 2 (label + relabel rule) | 0 | **0** |
| Silent-failure modes the tenant can introduce | several | n/a | **none** (miss is a signal) |
| Mutates tenant-owned resources | no | **yes** | **no** |
| Pod rollout on first reconcile | no | **yes** | no |
| GitOps drift / fight | no | **yes** | no |
| Failure observability | silent fallback | n/a | counter + status condition |
| Kind coupling | wherever tenant labels | write paths per kind | one mapper per kind (already needed for scale-target reads) |
| Controller watch/memory cost | none new | none new (writes templates) | **new Pod informer — bounded by selector + transform** (see [Watch & Memory Cost](#watch--memory-cost)) |
| Adoption friction | high (Prometheus relabeling knowledge) | low | **lowest** (zero WVA-specific YAML) |
| Forward-compatible with VA deprecation | awkward (label tracks a deprecated name) | awkward (controller writes a label whose anchor is being deprecated) | **clean** (cache value type abstracts the discovery anchor) |
| Composes with the config-ux persona contract | partial (tenant authors WVA-specific YAML) | conflicts (controller mutates tenant resources) | **fully** (WVA reads, never writes) |

---

## Open Questions

- **Watch & memory cost.** Addressed in [Watch & Memory Cost](#watch--memory-cost): the Pod informer is **new** (WVA watches no pods today), the dominant cost is the full-object Pod cache in cluster-scoped mode, and the mitigation is a scoped informer (label/field selector + transform) shipped in Phase 1, following the label-selector pattern already used for WVA's ConfigMap cache. Open sub-question: the exact selector — workload pod-template labels vs. restricting to VA-bearing namespaces — to be settled with a memory benchmark on a representative cluster.
- **Custom workload kinds.** The opt-in label covers them. No regression versus the post-#1145 state.
- **Rolling updates.** Eviction on `Pod` delete + repopulation on the new pod's add event is straight-line. The brief overlap window matches the same window the current label-based path faces between pod creation and the first scrape.
- **Interaction with the `PodMappingMissing` condition once VAs disappear.** Post-deprecation, this becomes an event on the discovery owner (`ScaledObject`/`HPA`) rather than a CRD condition. The event payload and observability surface are settled when Phase 3 lands.
