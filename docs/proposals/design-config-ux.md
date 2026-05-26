# Proposal: ScalingPolicy CRD for WVA configuration

---

## Problem Statement

WVA configuration is spread across six layers: four ConfigMaps (`wva-variantautoscaling-config`, `wva-saturation-scaling-config`, `wva-queueing-model-config`, `wva-model-scale-to-zero-config`), the `VariantAutoscaling` CRD spec, annotations on `ScaledObject`/`HPA`/Namespace, ~25 environment variables, Helm values, and implicit surfaces like hardcoded `ServiceMonitor` names. Each surface was added for a good reason at the time. The net result is roughly 60 distinct configuration knobs scattered across six locations, with no single way to discover, validate, or override them.

An operator asking "what policy applies to model `granite-13b` in namespace `prod`?" must inspect a global ConfigMap, a possible namespace-local override of the same ConfigMap, a separate scale-to-zero ConfigMap, the queueing-model ConfigMap, annotations on the `ScaledObject`, fields on the VA CRD, and environment variables on the controller. No single `kubectl` command returns the effective policy.

Validation timing is also inconsistent: VA CRD spec fields are admission-validated by OpenAPI; ConfigMap fields are validated by WVA on parse (errors surface in controller logs); annotations are validated by the controller after creation (no admission gate). Typos surface at different points through different channels.

Anticipated growth makes this worse, not better. Issue #1002 will add another ConfigMap surface for quotas. PR #1052 already added per-analyzer thresholds. The companion VA CRD deprecation pushes *more* config into annotations. Without a deliberate shape, surface count grows monotonically.

This proposal introduces a single typed CRD as the source of truth for scaling policy, while leaving discovery on annotations (per the VA CRD deprecation) and infrastructure config on env vars / Helm. Three concerns, three surfaces, no overlap.

### Context: the VariantAutoscaling CRD is being deprecated

The companion proposal `docs/proposals/deprecate-va-crd.md` removes the VA CRD as the user-facing API and moves variant *discovery* + per-workload metadata to annotations on `ScaledObject`/`HPA`. That proposal addresses discovery cleanly. It does not address *policy* — thresholds, analyzer weights, scale-to-zero, queueing-model parameters, quota — which today still live in ConfigMaps. This proposal picks up where the deprecation leaves off.

---

## Design Philosophy

Three concerns, three surfaces. Each surface has one purpose, one owner, one precedence rule.

| Concern | Surface | Owner |
|---|---|---|
| Discovery (which workloads WVA manages) and per-workload identity | Annotations on `ScaledObject`/`HPA` | Namespace owner |
| Scaling policy: thresholds, analyzers, scale-to-zero, queueing-model | `ScalingPolicy` CRD (this proposal) | Namespace owner |
| Quota: per-namespace GPU caps | `ScalingPolicy` cluster default only | Cluster admin |
| Infrastructure: Prometheus URL, TLS, leader election, log level | Env vars + Helm values | Cluster admin (install-time) |

Discovery is namespace-owned because the operator deploying a model knows which model it is. Threshold tuning is namespace-owned because each team knows their own workload. Quota is cluster-admin-owned because allocation governance flows from above. Infrastructure is install-time because it doesn't change per-workload.

The `ScalingPolicy` CRD is the only new surface this proposal introduces.

---

## Goals

1. Single typed CRD as the source of truth for scaling policy
2. Admission-time validation (OpenAPI + CEL) for every policy field
3. Native K8s RBAC enforces the governance split between thresholds (namespace owner) and quota (cluster admin)
4. One precedence rule across all policy fields: per-pool → per-namespace default → cluster default
5. Composes cleanly with the VA CRD deprecation, the quota limiter (#1002), and the throughput analyzer (#1052) without adding new schemas later

## Non-Goals

- Replacing infrastructure config (Prometheus URL, TLS, leader election) — those stay in env vars / Helm
- Replacing variant-discovery annotations from the VA CRD deprecation
- Removing ConfigMaps in one shot — a non-breaking phased rollout is part of the plan
- Multi-cluster federation, custom analyzer authoring UX, observability tuning — out of scope

---

## Proposed Solution

### One Namespaced CRD: `ScalingPolicy`

A single namespaced CRD under `scaling.llm-d.ai/v1alpha1` carries every policy concern. The match key is `spec.poolRef` — a `LocalObjectReference` to the `InferencePool` (from the Gateway API inference extension) whose pods the policy applies to. WVA always runs alongside llm-d, which ships the inference extension, so `InferencePool` is always installed and watchable.

Operators never type model identifiers into a policy spec. The controller derives `status.modelID` from the referenced pool and exposes it via a `+kubebuilder:printcolumn` so `kubectl get scalingpolicy -n production` shows both the matched pool and the model.

```yaml
apiVersion: scaling.llm-d.ai/v1alpha1
kind: ScalingPolicy
metadata:
  name: granite-premium-priority      # name describes purpose, not identity
  namespace: production
spec:
  poolRef:
    name: granite-premium-pool        # InferencePool in the same namespace
  saturation:
    scaleUpThreshold: 0.95
  scaleToZero:
    enabled: false
  priority: 2.0
status:
  modelID: ibm/granite-13b            # derived by controller
```

The controller discovers which `InferencePool` a workload's pods belong to by watching `InferencePool` objects and matching their `spec.selector` against the workload's pod template labels. Operators do not author the pool-to-workload mapping; it is derived from the inference-extension API.

### Three-Tier Resolution

For each workload, the controller resolves a policy via three deterministic lookups, in fixed order:

1. **Per-pool override** — `ScalingPolicy` in the workload's namespace with `spec.poolRef.name` matching the workload's pool.
2. **Namespace default** — `ScalingPolicy` in the workload's namespace with `spec.poolRef` absent.
3. **Cluster default** — `ScalingPolicy` in the system namespace (default `workload-variant-autoscaler-system`, configurable via `--system-namespace`) with `spec.poolRef` absent.

Fields merge from cluster default → namespace default → per-pool override. Scalars: higher tier wins if set. Maps: overlay per key. Lists with a natural identifier (`spec.analyzers[*]` keyed by `name`): merge by name — lower-tier entries inherited unless the higher tier overrides them, higher-tier entries added when no lower-tier match exists. Lists without an identifier: replace wholesale.

The resolved policy is published as `status.effectivePolicy` on the workload along with the names of the three source `ScalingPolicy` objects. `kubectl describe scaledobject foo` shows the merged values and their sources directly — no detective work.

### Quota Lives Only at the Cluster Default

Quota is governed top-down: cluster admins set per-namespace GPU caps; namespace owners cannot relax them. The CRD schema reflects this directly:

```yaml
# Only this object accepts spec.quota. RBAC on the system namespace
# ensures only cluster admins can write here.
apiVersion: scaling.llm-d.ai/v1alpha1
kind: ScalingPolicy
metadata:
  name: default
  namespace: workload-variant-autoscaler-system
spec:
  saturation: { ... }                 # threshold defaults
  quota:
    default:                          # for namespaces not listed below
      perGPU: { H100: 8 }
    perNamespace:
      production:
        perGPU: { H100: 16, A100: -1 }   # -1 = unlimited
      development:
        perGPU: { H100: 4 }
```

A CEL `x-kubernetes-validations` rule rejects `spec.quota` on any `ScalingPolicy` outside the system namespace's `default` object. Namespace owners cannot create a policy that contains `spec.quota`; the rule fires at admission with a clear error message. Silent ignoring would be a footgun — operators would set the field thinking it applies and not understand why their cap isn't enforced.

K8s-native namespace RBAC enforces the governance split: cluster admins have write access to the system namespace; namespace owners have write access only to their own. No custom admission webhook is required for the RBAC dimension; CEL handles the field-level rule.

For orgs delegating quota editing to a platform team without granting full cluster-admin, WVA ships a `quota-editor` `ClusterRole` granting `update`/`patch` access to the cluster-default `ScalingPolicy` only (scoped via `resourceNames: ["default"]`). The delegation is expressible in standard K8s RBAC; no second CRD is needed.

### Per-Pool Keying Handles Per-Role for Free

WVA today configures thresholds and quotas per-model. The platform direction is per-model-per-role `InferencePool`s — one pool per role (prefill, decode). After that transition, each role-pool gets its own `ScalingPolicy` via `poolRef`. No `spec.role` or `spec.perRole` field is needed; per-pool keying inherits per-role semantics.

Per-role quotas are also unnecessary at the schema level. Cluster admins set namespace caps in GPUs (`H100: 16`); the scaling engine allocates those GPUs across the namespace's per-role pools based on per-pool demand and priority. Role-aware quota differentiation is expressed via per-pool `priority` and `scaleBounds`, not a separate quota dimension.

If WVA ever needs to scale roles independently within a single mixed pool (unlikely given the per-role-pool transition), `spec.perRole` can be added as a non-breaking additive change at that time.

### Conflict Detection and Bootstrap

Two `ScalingPolicy` objects in the same namespace cannot reference the same `InferencePool`. A CEL list-uniqueness rule on `spec.poolRef.name` rejects duplicates at admission. CEL `x-kubernetes-validations` went GA in K8s 1.30; the lowest K8s version supported elsewhere in the llm-d stack (1.32+ via the inference extension's "last three minors" policy) is well above that floor, so no controller-side fallback or admission webhook is needed.

The cluster-default `ScalingPolicy` is mandatory. When it is missing, the controller refuses to start and emits a `Bootstrap` warning event. The cluster default is the only source of quota; falling back to hard-coded built-ins would hide quota policy behind invisible defaults, which is the failure mode this proposal exists to prevent. Namespace defaults and per-pool overrides remain optional and fall through to the next tier normally when absent.

---

## What Does NOT Change

- Discovery via `ScaledObject`/`HPA` annotations (per the VA CRD deprecation)
- Per-workload metadata annotations (`llm-d.ai/managed`, `llm-d.ai/model-id`, `llm-d.ai/variant-cost`, `llm-d.ai/role`)
- Namespace-level controls (`wva.llmd.ai/config-enabled`, `wva.llmd.ai/exclude`)
- Infrastructure configuration (Prometheus URL, TLS, leader election, log level — still in env vars + Helm values)
- vLLM/EPP metric collection and Prometheus queries
- Analyzer pipeline (saturation V1/V2, queueing model, throughput)
- Optimizer (cost-aware, greedy-by-score)
- `wva_desired_replicas` exposure and KEDA/HPA consumption

---

## Migration Path

### What Changes for Users

| Before | After |
|---|---|
| 4 ConfigMaps (`wva-*-config`) | 1+ `ScalingPolicy` objects |
| Per-model overrides via `{modelID}#{namespace}` map keys | Per-pool `ScalingPolicy` objects, matched by `spec.poolRef` |
| Threshold typos surface in controller logs | Threshold typos rejected at `kubectl apply` |
| Quota planned as a separate ConfigMap (#1002) | `spec.quota` on the cluster default `ScalingPolicy` |
| Discoverability: read four ConfigMap YAMLs | `kubectl describe scaledobject foo` → `status.effectivePolicy` |

### Migration Tool

A `wva-config migrate` CLI subcommand reads existing ConfigMaps and emits equivalent `ScalingPolicy` YAML:

```bash
wva-config migrate --dry-run
wva-config migrate --apply
```

The tool maps the cluster-default ConfigMap entries to a system-namespace `ScalingPolicy default`, per-model overrides to per-pool policies in the corresponding namespaces, and (once #1002 lands) the quota ConfigMap to `spec.quota` on the cluster default.

---

## Implementation Phases

### Phase 1: ScalingPolicy CRD alongside ConfigMaps (Non-Breaking)

**Goal:** Introduce the CRD and resolver; ConfigMaps continue to work unchanged.

**Deliverables:**
- `ScalingPolicy` CRD under `scaling.llm-d.ai/v1alpha1` with the full schema covering today's three policy ConfigMaps
- CEL validation rules: (a) `spec.quota` only on cluster default, (b) `spec.poolRef.name` uniqueness within a namespace
- `PolicyResolver` performing the three-tier lookup; controller reads from CRDs if present, falls back to ConfigMaps otherwise
- `status.effectivePolicy` on managed `ScaledObject`/`HPA`
- `quota-editor` `ClusterRole` in default RBAC manifest
- `--system-namespace` controller flag (default `workload-variant-autoscaler-system`)

**Success Criteria:** Either ConfigMap-only or CRD-only configurations produce identical scaling decisions; mixed mode resolves cleanly via the documented precedence.

### Phase 2: Deprecation

**Goal:** Mark ConfigMaps deprecated; default install ships CRDs.

**Deliverables:**
- `wva-config migrate` CLI tool
- Default samples in `config/samples/` use the CRD
- Deprecation warning event emitted when WVA reads from a ConfigMap
- `docs/user-guide/scaling-policy.md` describes the three-tier model and quota governance

**Success Criteria:** All sample and documentation paths use the CRD; CI tests pass with ConfigMaps absent.

### Phase 3: ConfigMap Removal

**Deliverables:**
- Remove ConfigMap loader code from `internal/config/`
- Remove ConfigMap reconciliation from `internal/controller/`
- Retain analyzer, optimizer, collector, and engine code (unchanged)

**Success Criteria:** WVA binary no longer reads scaling-policy ConfigMaps. All functionality available via `ScalingPolicy`.

---

## Alternatives Considered

1. **Status quo (4 ConfigMaps + CRD spec + annotations + env vars + Helm).** Defensible until the surface count exceeds operators' working memory. At six layers, the cumulative discoverability and validation cost outweighs the migration cost.

2. **Collapse the four ConfigMaps into one.** Cheap. Doesn't address validation timing or schema discoverability. Strictly worse than this proposal on every dimension except "ConfigMaps already exist."

3. **All per-pool policy on `ScaledObject`/`HPA` annotations (Knative-style).** Works for low knob counts; degrades as analyzer thresholds, quota, and SLO config arrive. Cross-cutting changes ("priority=2 for all prod models") force per-workload edits. The Knative community's well-documented regret about its 30+ autoscaling annotations is the cautionary case study.

4. **Two scoped CRDs (`ClusterScalingPolicy` + `ScalingPolicy`).** Forces operators to decide "is this a cluster fact or a namespace fact?" up front. The single-CRD design defers that decision into the tier the policy sits in. Adding scope is a one-line edit, not a "delete here, recreate there" migration.

5. **One cluster-scoped CRD with `namespaceSelector` + `weight` (Karpenter `NodePool` shape).** Necessary if WVA needed "all `tier=prod` namespaces inherit X" without enumerating namespaces. Most clusters have a handful of namespaces, not hundreds. The selector-and-weight engine carries permanent complexity (conflict resolution, status reporting) that the three-tier name lookup avoids.

6. **Annotated `ResourceQuota` for per-GPU-type quotas.** Tempting (K8s-native, no new CRD), but K8s admission does not interpret the annotation. Multiple `ResourceQuota` objects on the same `requests.nvidia.com/gpu` counter compound as a logical AND, giving `min(hard_i)` instead of per-type caps. Per-GPU-type quotas at admission require DRA (K8s 1.34+, KEP #4840 still maturing). WVA can additively *read* `ResourceQuota` as a complementary input in a future PR (per design-1002's A2), but not as a substitute for `spec.quota`.

7. **`spec.modelID` as the match key (with `metadata.name` as a sanitized DNS-safe slug).** Earlier draft of this proposal. Rejected once we accepted llm-d-as-stack: the `InferencePool` reference is always available, exact-match, and post-transition naturally encodes role. `status.modelID` is preferable to `spec.modelID` because the model identity is derivable, not an authoritative input.

8. **Helm values as the policy surface.** Tightly couples deploy-time and runtime; requires `helm upgrade` per policy change.

See `design-config-ux-analysis.md` for the full strengths/weaknesses breakdown of each alternative.

---

## Comparison Matrix

S1 (this proposal) compared with the alternatives above. Each cell summarises the option's behavior on a property the proposal's goals call out; `design-config-ux-analysis.md` carries the extended matrix with more dimensions.

| Property | A: status quo | B: one ConfigMap | C: flat annotations | D: two CRDs | E: D + annotations | S2: cluster CRD + selectors | **S1: this proposal** |
|---|---|---|---|---|---|---|---|
| Admission-time validation | ❌ | ❌ | ❌ | ✅ | partial | ✅ | ✅ |
| Single source of truth | ❌ (6 layers) | partial | ✅ per workload | ✅ per scope | ✅ with precedence | ✅ | ✅ |
| Kinds to learn for policy | 4 ConfigMaps | 1 ConfigMap | 0 (annotations only) | 2 CRDs | 2 CRDs + annotations | 1 CRD | 1 CRD |
| `kubectl explain` for policy | partial | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| K8s-native RBAC governance split | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| Cross-cutting changes | ConfigMap edit | ConfigMap edit | every `ScaledObject` | one CRD edit | one CRD edit | one selector edit | one Kustomize patch |
| Per-GPU-type quota | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| Per-pool granularity | ❌ | ❌ | partial | partial | partial | ✅ (selector) | ✅ (`poolRef`) |
| Selector engine complexity | n/a | n/a | n/a | n/a | n/a | permanent | none |
| Net new objects to install | 0 | 0 | 0 | 2 CRDs | 2 CRDs + annotations | 1 CRD | 1 CRD |

S1 wins on the dimensions that the proposal's goals call out — admission-time validation, K8s-native RBAC, single kind to learn — while avoiding the selector engine S2 carries and the singleton bottleneck a Karpenter-style single-object design would impose. Where another option ties on a row, S1 ties; where S1 wins, it's on kind count, governance ergonomics, or absence of a separate annotation surface.

---

## Autoscaler Comparison

How peer Kubernetes autoscalers shape their configuration UX:

| System | Primary surface | Defaults layer | Per-workload override |
|---|---|---|---|
| **Kubernetes HPA** | CRD spec only | none (kube-controller-manager flags) | every HPA carries its full spec |
| **KEDA** | `ScaledObject` CRD + ~5 annotations on the same object | none | the `ScaledObject` itself |
| **Knative KPA** | `config-autoscaler` ConfigMap + per-Revision annotations (~30 keys) | ConfigMap | annotations on Revision template |
| **Kueue** | `ClusterQueue` + `LocalQueue` + `ResourceFlavor` + `Workload` | `ClusterQueue` + `ResourceFlavor` | `LocalQueue` references `ClusterQueue`; per-`Workload` priority/preemption |
| **Karpenter** | `NodePool` (cluster-scoped) + `EC2NodeClass` | `NodePool.spec.weight` priority | per-pod tolerations / `nodeSelector` |
| **KServe** | `InferenceService` CRD spec + Knative annotations | Knative `config-autoscaler` | per-`InferenceService` annotations |
| **AWS Application Auto Scaling** | per-resource `ScalingPolicy` API objects | none — every policy explicit | per-resource |

Three patterns recur across the peer set:

1. **Two layers is the dominant shape.** Knative, KServe, and Kueue all run a cluster-defaulting layer plus a per-workload override layer. Single-layer designs (HPA, KEDA, AWS App Auto Scaling) force per-workload boilerplate.
2. **Typed CRDs carry the heavy schema; annotations cover the hot knobs.** Knative is the closest analog to WVA's mix of analyzer thresholds and emergency overrides. The Knative community now actively *resists* growing the annotation list because each new annotation is a stringly-typed concept.
3. **Discovery and policy are separate surfaces.** KEDA uses `ScaledObject` for both — the design WVA's VA CRD deprecation moves *toward*. Knative/KServe split discovery (`Service`/`InferenceService`) from policy (annotations + ConfigMap). The split shows up positively in retrospective analyses.

The 2024-2026 direction is consistent: **typed CRDs for steady-state policy, annotations as a small escape valve, install-time config for infrastructure**. WVA's design tracks that direction with one departure — name-based three-tier lookup instead of selectors, because WVA's policy granularity is namespace-shaped not cluster-shaped, so selectors are unnecessary complexity.
