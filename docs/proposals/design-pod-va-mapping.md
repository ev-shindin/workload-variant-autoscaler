# Proposal: Pod → `VariantAutoscaling` Mapping via Reconcile-Time Derivation

---

## Problem Statement

WVA must associate every vLLM Prometheus metric with the `VariantAutoscaling` that owns the pod producing it. Per-pod metrics (KV-cache usage, queue depth, latencies, cache config) feed per-VA scaling decisions on every reconcile; the mapping sits on the critical path of the autoscaler.

The current implementation, landed by PR #1145 (merged 2026-05-19), is **tenant-driven**. Per `deploy/README.md:68`, to make one `VariantAutoscaling` actually receive metrics, a tenant must author **two coordinated artifacts** beyond the workload itself:

1. `llm-d.ai/variant: <VA-name>` on the pod template's `metadata.labels`. For `LeaderWorkerSet` the label has to be set on *both* `leaderTemplate` and `workerTemplate`. The value must equal the `VariantAutoscaling`'s `metadata.name` exactly — there is no admission check on the link.
2. A `metricRelabelings` rule on the ServiceMonitor/PodMonitor that propagates `__meta_kubernetes_pod_label_llmd_ai_variant → llm_d_ai_variant` into the Prometheus time series.

Each is an independent silent-failure mode. A wrong label key, a value drifted from the VA name, a missing relabeling rule, a half-templated LWS, the wrong monitor scope — any one yields zero matching metrics. The collector then falls through to `computeReplicaCapacityFallback` (the same fallback path the `cache_config_info` bug exposed in PR #1198) and the autoscaler produces "working" but quantitatively wrong scaling. The tenant sees no error.

### Requiring a pod-template label is not how autoscalers attach

Labeling pods and relabeling metrics is common practice in general — but it is specifically uncommon for an **autoscaler**. HPA, KEDA, VPA, and the Cluster Autoscaler all bind to a workload through `scaleTargetRef` and read the target; none require editing the workload's pod template to be scaled. WVA already takes a `scaleTargetRef` exactly like HPA, so the required label is the one thing that breaks the norm.

It also blocks **in-place adoption.** To point WVA at a `Deployment` already running in production, the tenant must first edit `spec.template.metadata.labels` — a pod-template change, i.e. a **rolling restart of the whole workload** (and a GitOps source edit) *before autoscaling even starts working*. An autoscaler should attach to an existing workload without touching it; the label requirement makes WVA non-adoptable in place.

And the required label is not a self-describing workload label like `app=`/`role=` — it is a **WVA-internal join key** whose value must equal the `VariantAutoscaling`'s name, maintained by hand for the controller's benefit. Two things compound that: every part is a silent-failure mode, and the value's anchor — the VA name — is being deprecated (`deprecate-va-crd.md`), so the contract points at a referent that is going away.

Issue #1072 names the same fragility but proposes the opposite direction: have the *controller* stamp the label onto pod templates. That trades tenant friction for new costs — rolling restarts when the controller first reconciles a pre-existing workload, GitOps drift, mutation of tenant-owned resources, and kind-specific *write* paths replacing the kind-specific *read* paths #1145 removed. PR #1145 took neither extreme: it kept WVA read-only and asked the tenant to pay the friction.

### Context: PR #1145 and VA CRD deprecation

Three decisions shape this proposal:

- **PR #1145** introduced the required label, added the ServiceMonitor relabeling infrastructure, and **deleted** `internal/collector/source/pod_va_mapper.go` (142 lines + 555-line test). The codebase currently has no read-side derivation of pod→VA; it relies exclusively on the tenant-supplied label.
- **`docs/proposals/deprecate-va-crd.md`** is moving variant *discovery* from the `VariantAutoscaling` CRD onto annotations on `ScaledObject`/`HPA`. After deprecation, the `<VA-name>` anchor that the required label's value tracks no longer has a stable referent.
- **`docs/proposals/design-config-ux.md`** commits to a persona contract — *tenants author their workload + policy; WVA reads, never writes* — that the current required-label-plus-relabel-rule contract partially undermines.

---

## Design Philosophy

WVA derives the pod→VA mapping inside the controller, from the scale target it already reads — listing each target's pods and resolving them to the owning VA at reconcile time, off the metric hot path. The tenant authors nothing WVA-specific in the workload's YAML. The controller mutates nothing the tenant owns. The fragility cited in #1072 (race conditions, label-name inconsistency, silent drops) is real but **implementation-level** — it is solvable inside WVA, not by requiring coordinated authoring artifacts from the tenant, and not by mutating their `Deployment`/`LWS`.

The map exposes a single internal API — `Lookup(podKey) → discovery key` — which the rest of the controller depends on. What that key *means* today is a `VariantAutoscaling`; what it means after VA deprecation is "the discovery source for this pod" (an annotated `ScaledObject`/`HPA`). Same API, different value type. The proposal lands a stable internal contract that absorbs the discovery-surface migration without API churn.

---

## Goals

1. Zero new authoring requirement on the tenant for the default mapping path — WVA attaches to an existing workload without a pod-template edit or rollout, like a standard autoscaler.
2. No mutation of tenant-owned resources (`Deployment`, `LWS`, `ServiceMonitor`).
3. Bounded controller cost — no cluster-scale Pod cache and no always-on Pod watch (the mapping is derived at reconcile time).
4. Misconfigurations are visible, not silent — a metric that can't be resolved surfaces instead of degrading into wrong-capacity fallback.
5. Forward-compatible with the VA CRD deprecation — consumers don't change when discovery moves from the CRD to annotations on `ScaledObject`/`HPA`.
6. No regression for topologies the derivation cannot resolve (custom workload kinds, non-standard composition) — they remain supported via the opt-in `llm-d.ai/variant` label.

## Non-Goals

- Reverting PR #1145 — multi-vLLM-per-pod support (`buildInstanceKey`), the ServiceMonitor relabeling infrastructure, and the test updates stay. This is a corrective follow-up, not a revert (see Alternatives Considered #3).
- Implementing controller-side mutation of pod templates (the direction #1072 proposes). This proposal rejects that direction explicitly.
- Adding admission-time validation that the optional escape-hatch label's value tracks a VA name. The derivation is the source of truth; the escape hatch is best-effort.
- Replacing the ServiceMonitor / PodMonitor configuration — the relabeling rule in `config/prometheus/vllm-servicemonitor.yaml` stays for installs that keep the opt-in label.

---

## Proposed Solution

### Reconcile-Time Pod→VA Derivation

A new `internal/collector/source/pod_va_map.go` builds an in-memory map keyed by pod (`namespace/name`, or `namespace/name:port` for multi-vLLM pods) whose value is the discovery key — a `VariantAutoscaling` namespaced key today, an annotated-workload key after VA deprecation.

The map is **rebuilt at reconcile time**, not maintained by an always-on Pod informer. WVA already reaches each managed workload through `scaleTargetRef` (on the `VariantAutoscaling` today, on the annotated `ScaledObject`/`HPA` after deprecation) and reads the target's pod-template selector. Once per reconcile, for each managed scale target, WVA lists the pods matching that selector and resolves each to that target's `VariantAutoscaling` — confirming ownership through the pod's owner references, so a stray pod that merely shares the selector labels is not misattributed. This reuses the on-demand pod LIST the pod-scraping source already performs and adds **no new always-on watch** (see [Watch Cost](#watch-cost)).

The decisive change is *where* resolution runs: **once per reconcile per pod** while building the map, not the per-metric pod→owner→VA walk the read-side resolver did before #1145. Lookups on the metric hot path are a single O(1) map read, with no Kubernetes API calls in the metrics-collection path.

### Hot-Path Lookup

`internal/collector/replica_metrics.go`'s per-metric processing becomes a single map lookup. There are no Kubernetes API calls in the metrics-collection path. The repeated `pod`/`pod_name` extraction scattered through the current collector collapses into one `extractPodKey(labels)` helper at the boundary, normalizing the two label-name conventions in a single place.

### Misses as a Signal

Two complementary signals replace the silent fallback:

- A metric that resolves to no managed pod increments `wva_pod_va_map_miss_total{reason}` and logs a **structured warning** at the `computeReplicaCapacityFallback` boundary — the unresolved pod, the labels it carried, and the key expected. These key on the *pod*, so they fire even when the pod belongs to no `VariantAutoscaling`.
- At reconcile time, a `VariantAutoscaling` whose scale target has pods but whose metrics are not resolving gets a `PodMappingMissing` status condition (post-deprecation: an event on the discovery owner). This is computed from the VA's side — its known pods versus the metrics seen — so it always names the right owner, which an orphan metric cannot.

The dominant cause is the brief window between pod creation and the next reconcile's list — typically shorter than the first scrape interval — so most are transient. The point is that they are now *visible* (counter + log + condition), not silent fallbacks.

### Optional Tenant-Supplied Override

`VariantLabelKey = "llm-d.ai/variant"` stays in `internal/constants/labels.go`. When present on a pod's metric labels, WVA reads it and uses it with *higher precedence* than the derivation. This covers custom workload kinds and non-standard compositions the selector-based derivation can't resolve (e.g., a scale target whose selector doesn't map cleanly to the serving pods). WVA never writes the label; the ServiceMonitor relabeling rule continues to propagate it for installs that choose the opt-in path.

### Tenant-Facing UX

Author a `VariantAutoscaling` referencing a `Deployment`/`LWS` — done. The same shape as `HorizontalPodAutoscaler` against a `Deployment`. No required label, no relabeling rule, no LWS double-template, no Prometheus relabeling knowledge, and no pod-template edit or rollout to adopt. After VA deprecation, the same applies with discovery moved to annotations on `ScaledObject`/`HPA`.

---

## How This Resolves the Fragility

Every silent-failure mode in the Problem Statement comes from the tenant hand-maintaining a join key the controller could derive. The design removes each at its root — by not requiring the join key — rather than detecting it after the fact:

| Failure mode today | Why it cannot happen |
|---|---|
| Wrong / typo'd label key | No label on the default path. Identity comes from `scaleTargetRef` — a typed, defaulted field — not a free-form string retyped on each pod template. |
| Label value drifts from the VA name | Nothing to keep in sync. The `VA → scaleTargetRef → pods` link *is* the source of truth; there is no second copy of the VA name to drift. |
| Missing ServiceMonitor relabel rule | Not needed on the default path — the mapping no longer travels through a Prometheus label. |
| Half-templated `LeaderWorkerSet` (label on one template only) | No per-template label to forget. |
| Pod-creation / sync race | Becomes a transient map miss that self-heals on the next reconcile — and is counted and logged, not silent. |

The one thing the design does **not** remove is the generic prerequisite that a `ServiceMonitor`/`PodMonitor` scrapes the pods (a wrong monitor scope still yields no metrics). But even that stops being *silent*: a VA whose pods produce no resolvable metrics raises `PodMappingMissing` instead of falling through to wrong-capacity scaling. The through-line is that the WVA-specific failure modes are eliminated at the source, and the one residual prerequisite is turned into a visible signal rather than a quiet wrong answer.

---

## Watch Cost

The derivation adds **no new always-on watch.** WVA's controllers already watch `VariantAutoscaling` (primary), `ServiceMonitor`, `Deployment`, and `LeaderWorkerSet` (`internal/controller/variantautoscaling_controller.go`), plus `ConfigMap`, `HorizontalPodAutoscaler`, `ScaledObject`, and `InferencePool` in their own reconcilers — **none watch `Pod`**, and this proposal does not add a Pod informer. The map is built from a **bounded pod LIST per reconcile**, scoped to the pods behind each managed `scaleTargetRef`'s selector — the same on-demand list the pod-scraping source already issues, not a continuous cluster-wide Pod cache. There is no full-`Pod`-object informer cache, and therefore none of the cluster-scale memory cost one would add. (This is the deliberate change from an earlier informer-backed design, which would have introduced WVA's first always-on Pod watch.)

**Scoping the list.** The list is bounded by the workload's own pod-template selector — labels the workload **already carries** (read from the scale target), *not* a new WVA-specific pod label, which would reintroduce the tenant-labeling dependency this proposal removes. WVA reaches that selector through `scaleTargetRef`, which exists on the `VariantAutoscaling` today (`spec.scaleTargetRef`, `api/v1alpha1/variantautoscaling_types.go`) and on the annotated `ScaledObject`/`HPA` after deprecation (per [`deprecate-va-crd.md`](deprecate-va-crd.md): WVA resolves the target from the discovery owner's `spec.scaleTargetRef`). The selector source is therefore **stable across the VA-deprecation migration** — only the object holding the `scaleTargetRef` changes, not the mechanism.

For very large clusters, restricting the per-reconcile list to namespaces that contain a `VariantAutoscaling` (the HPA/ScaledObject reconcilers already track this set) bounds it further.

---

## What Does NOT Change

- The `VariantLabelKey = "llm-d.ai/variant"` constant in `internal/constants/labels.go` (now opt-in, not required).
- The `VariantLabelPrometheusKey = "llm_d_ai_variant"` constant and the `metricRelabelings` rule in `config/prometheus/vllm-servicemonitor.yaml` (used by the opt-in path).
- PromQL query templates' `by (instance, pod, llm_d_ai_variant, …)` grouping clauses — harmless when the variant label is absent, meaningful when the opt-in path is in use.
- vLLM / EPP metric collection, the analyzer pipeline (saturation V1/V2, queueing model, throughput), the optimizer, and the `wva_desired_replicas` output.
- The `VariantAutoscaling` API itself (until VA deprecation lands).
- The requirement that a `ServiceMonitor`/`PodMonitor` actually scrapes the vLLM pods — a generic Prometheus prerequisite, not WVA-specific coordination. This proposal removes the per-VA label/relabel *join*, not the need for the metrics to exist; a monitor that selects no pods still yields no metrics (and nothing to resolve).

---

## Migration Path

### What Changes for Users

| Before (current, post-#1145) | After |
|---|---|
| Add `llm-d.ai/variant: <VA-name>` to every Deployment pod template (a pod-template edit → rolling restart to adopt) | No required label; nothing to author, no rollout |
| Add the same label to *both* `leaderTemplate` and `workerTemplate` on LWS | Same — no label needed |
| Ensure ServiceMonitor / PodMonitor has the `__meta_kubernetes_pod_label_llmd_ai_variant → llm_d_ai_variant` relabeling rule | Rule still installed; required only if using the opt-in override |
| Silent metric drop → wrong-capacity fallback on misconfiguration | `wva_pod_va_map_miss_total` + `PodMappingMissing` condition + structured log |
| Cannot use WVA against workloads the tenant cannot label | Default path works for any supported workload kind; opt-in label remains for the long tail |

### Migration Tool

None required. The change is non-breaking: installs that retain `llm-d.ai/variant` continue to work via the opt-in precedence path, and the derivation resolves transparently for workloads that drop it.

---

## Implementation Phases

### Phase 1: Reconcile-time derivation alongside the existing label path (Non-Breaking)

**Goal:** Add the derivation; both paths work; the tenant-supplied label takes precedence when present.

**Deliverables:**

- `internal/collector/source/pod_va_map.go` that rebuilds the `podKey → VariantKey` map at reconcile time by listing the pods behind each managed `scaleTargetRef`'s selector and resolving them to that target's VA via owner-ref confirmation (once per reconcile, not per metric) — **no new always-on watch** (see [Watch Cost](#watch-cost)).
- `Lookup(podKey) → VariantKey` API plus a single `extractPodKey(labels)` helper at the collector boundary, replacing the per-metric `pod`/`pod_name` fallback in `internal/collector/replica_metrics.go`.
- `wva_pod_va_map_miss_total{reason}` counter, a `PodMappingMissing` status condition on `VariantAutoscaling`, and a structured warning at the `computeReplicaCapacityFallback` boundary.
- Documented opt-in precedence for the `llm-d.ai/variant` label: the map reads it when present, never writes it.

**Success Criteria:** A workload with no `llm-d.ai/variant` label, plus a `VariantAutoscaling` pointing at it, produces metrics matched to the VA without any additional configuration. Map-miss counter is zero in steady state.

### Phase 2: Documentation flip

**Goal:** Demote the label from "required" to "opt-in escape hatch" in user-facing docs.

**Deliverables:**

- `deploy/README.md` — remove the "WVA does not add this label automatically; you must…" wording and the LWS double-template instructions from the Workload Requirements section. Re-cast the label as an escape hatch with a brief "when you need this" rationale.
- `docs/design/controller-behavior.md` — same demotion in the Prerequisites section #1145 added. Add a troubleshooting entry for `PodMappingMissing`.
- Samples in `config/samples/` and fixtures in `test/e2e/fixtures/` — drop the now-unnecessary label from the default cases; retain one fixture that exercises the opt-in path.

**Success Criteria:** Default samples and e2e tests pass with the label absent from default fixtures.

### Phase 3: Compose with VA Deprecation

**Goal:** Map values become the abstract "discovery key" (a VA today, an annotated `ScaledObject`/`HPA` post-deprecation).

**Deliverables:**

- Map value type generalized; lookup callers updated.
- Status-condition / event emission generalized to the new owner kind.
- No tenant-visible change.

**Success Criteria:** Installs running on either the VA-CRD path or the annotation-discovery path resolve pod→discovery-source through the same map API.

---

## Alternatives Considered

*Two suggestions from review — a reconcile-time check and better structured logging — are folded into the design above (the map is built at reconcile time; misses emit a structured warning), so they are part of the proposal rather than alternatives.*

1. **Status quo: tenant-required label + ServiceMonitor relabeling rule (PR #1145 as merged).** Achieves "no API calls per metric" by demanding two coordinated authoring artifacts from the tenant, plus a pod-template edit (rolling restart) to adopt on an existing workload. The silent-fallback and in-place-adoption failure modes are the primary motivation for this proposal. Strictly worse on adoption friction and observability than C; equal on mutation-of-tenant-resources.

2. **Controller stamps `llm-d.ai/variant` on pod templates (issue #1072's proposal).** Zero tenant authoring burden, at the cost of (a) mutating tenant-owned `Deployment`/`LWS` `spec.template` (rolling restart on first reconcile, perma-drift in GitOps installs), (b) kind-specific *write* paths replacing the kind-specific *read* paths #1145 removed (no net simplification), and (c) direct conflict with the tenant-ownership persona of the config-ux proposal. The "fit with llm-d architecture" framing in #1072 also points the *other way* — the inference scheduler's P/D `role` label is set by the workload author, not by a controller.

3. **Revert PR #1145.** A full revert would also undo multi-vLLM-per-pod support, the `buildInstanceKey` helper, the ServiceMonitor relabeling infrastructure, the test updates, and the doc work — none of which this proposal disagrees with. The deleted `pod_va_mapper.go` was an *unhardened* version of what this proposal builds; recoverable from history if useful but small enough to rewrite. The corrective work is a follow-up PR layered on top of #1145, not a revert.

4. **Validating webhook / admission policy on the pod template (Option A from review).** A `ValidatingAdmissionPolicy` (GA in K8s 1.30) could reject a managed workload whose pod template is missing `llm-d.ai/variant`, surfacing typos at apply time. It converts one silent-failure mode into a loud one, but: it leaves the *requirement* itself in place (so the in-place-adoption and rolling-restart costs remain); CEL is single-object, so it can check the label is *present* but not that its value equals a `VariantAutoscaling` name (a cross-object link); and it validates the VA-name anchor that VA-deprecation removes. Useful as defense-in-depth for the opt-in label path, not a substitute for deriving the mapping.

5. **External sidecar / DaemonSet that watches pods and stamps the label out-of-tree.** Adds an operational component that does the same thing the controller already could. Rejected on complexity grounds — one less component is the better default.

---

## Comparison Matrix

| Property | A: Status quo (#1145) | B: Controller stamps (#1072) | **C: Reconcile-time derivation (this proposal)** |
|---|---|---|---|
| New things the tenant must author | 2 (label + relabel rule) | 0 | **0** |
| Attaches via `scaleTargetRef` like HPA/KEDA (no pod-template edit) | **no** | no (controller edits) | **yes** |
| Rollout to adopt on an existing workload | **yes** (tenant adds the label) | **yes** (controller rewrites template) | **no** |
| Silent-failure modes the tenant can introduce | several (label / value / relabel / LWS) | n/a | **WVA-specific ones removed** — a resolution miss is a signal |
| Mutates tenant-owned resources | no | **yes** | **no** |
| GitOps drift / fight | no | **yes** | no |
| Failure observability | silent fallback | n/a | counter + status condition + log |
| Kind coupling | wherever tenant labels | write paths per kind | one per-kind resolver — selector + owner refs (already needed for scale-target reads) |
| Controller watch/memory cost | none new | none new (writes templates) | **no new watch — bounded per-reconcile list** |
| Adoption friction | high (Prometheus relabeling knowledge) | low | **lowest** (zero WVA-specific YAML, no rollout) |
| Forward-compatible with VA deprecation | awkward (label tracks a deprecated name) | awkward (controller writes a label whose anchor is being deprecated) | **clean** (map value type abstracts the discovery anchor) |
| Composes with the config-ux persona contract | partial (tenant authors WVA-specific YAML) | conflicts (controller mutates tenant resources) | **fully** (WVA reads, never writes) |

---

## Open Questions

- **List scope and cost.** The map is built from a per-reconcile pod LIST scoped by the `scaleTargetRef` selector, plus owner-ref resolution per pod — no always-on watch. Open sub-questions: whether to additionally restrict to VA-bearing namespaces, and whether owner-ref confirmation needs a `ReplicaSet` read or can rely on the selector plus the pod's direct owner — settled with a list-latency benchmark on a representative cluster.
- **Custom workload kinds.** The opt-in label covers them. No regression versus the post-#1145 state.
- **Rolling updates.** A pod removed between reconciles is simply absent from the next rebuild; a new pod appears on the next reconcile's list. The brief overlap window matches the same window the current label-based path faces between pod creation and the first scrape.
- **Interaction with the `PodMappingMissing` condition once VAs disappear.** Post-deprecation, this becomes an event on the discovery owner (`ScaledObject`/`HPA`) rather than a CRD condition. The event payload and observability surface are settled when Phase 3 lands.
