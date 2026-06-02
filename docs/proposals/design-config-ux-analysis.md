# WVA Configuration UX — design analysis

**Companion to:** `design-config-ux.md` (the proposal)
**Status:** Draft
**Created:** 2026-05-25

This document records the detailed comparison work that motivates the `ScalingPolicy` CRD proposal. The proposal carries the final recommendation, the schema, and the implementation phases; this document carries the alternatives evaluation, the peer autoscaler survey, and the comparison matrix that the proposal cites but does not reproduce.

Read the proposal first. This document is the evidence trail.

---

## 1. Current configuration surfaces

WVA today exposes ~60 configuration knobs across six layers. Cataloguing them is the starting point for any UX redesign.

### ConfigMaps (4)

| Name | Scope | What it controls |
|---|---|---|
| `wva-variantautoscaling-config` | system namespace (default `workload-variant-autoscaler-system`), overridable via `CONFIG_MAP_NAME` | Prometheus connection, TLS, optimization interval, scale-to-zero flag, multi-controller isolation, node selector |
| `wva-saturation-scaling-config` | system + namespace-local (gated by `wva.llmd.ai/config-enabled` label) | Per-model saturation thresholds, V2 thresholds, analyzer list, priority, enableLimiter |
| `wva-queueing-model-config` | same as above | SLO multiplier, target TTFT/ITL, Kalman tuning toggle |
| `wva-model-scale-to-zero-config` | same as above | Scale-to-zero per-model enable, retention period |

Each ConfigMap has its own parsing helper (`internal/controller/configmap_helpers.go`), validation function, and defaulting (`ApplyDefaults`).

### VariantAutoscaling CRD spec

- `spec.scaleTargetRef` — Deployment/LWS/ScaledObject/HPA reference
- `spec.modelID` — model identifier (correlates with metric labels and ConfigMap keys)
- `spec.minReplicas` / `spec.maxReplicas`
- `spec.variantCost`

This CRD is being deprecated by `docs/proposals/deprecate-va-crd.md`; these fields move to annotations on `ScaledObject`/`HPA`.

### Annotations

| On | Annotation | Required | Purpose |
|---|---|---|---|
| `ScaledObject`/`HPA` | `llm-d.ai/managed` | yes | Opt into WVA |
| `ScaledObject`/`HPA` | `llm-d.ai/model-id` | yes | Model identifier |
| `ScaledObject`/`HPA` | `llm-d.ai/variant-cost` | no | Cost per replica |
| `ScaledObject`/`HPA` | `llm-d.ai/synthetic` | internal | Annotation-sourced VA marker |
| Namespace | `wva.llmd.ai/config-enabled` (label) | no | Opt namespace into local ConfigMap reads |
| Namespace | `wva.llmd.ai/exclude` | no | Exclude namespace from WVA management |

### Environment variables (~25)

Infrastructure: `PROMETHEUS_BASE_URL`, `PROMETHEUS_BEARER_TOKEN`, `PROMETHEUS_TLS_*`, `PROMETHEUS_METRICS_CACHE_*`, `LEADER_ELECTION_ID`. Feature flags: `WVA_SCALE_TO_ZERO`, `WVA_LIMITED_MODE`, `WVA_NODE_SELECTOR`, `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY`. System identity: `POD_NAMESPACE`, `CONFIG_MAP_NAME`, `SATURATION_CONFIG_MAP_NAME`, `QUEUEING_MODEL_CONFIG_MAP_NAME`, `CONTROLLER_INSTANCE`, `POOL_GROUP`.

### Command-line flags (~15)

`--metrics-bind-address`, `--health-probe-bind-address`, `--leader-elect`, `--leader-election-*`, `--rest-client-timeout`, `--metrics-secure`, `--enable-http2`, `--watch-namespace`, `-v`, `--webhook-cert-*`, `--metrics-cert-*`, `--config-file`.

### Helm values

Operator-tunable install-time knobs that template into the controller Deployment's env vars.

### Implicit surfaces

- Hardcoded `ServiceMonitor` name (`workload-variant-autoscaler-controller-manager-metrics-monitor`) checked by `internal/controller/predicates.go`
- `CONTROLLER_INSTANCE` propagated as a Prometheus label across every metric

---

## 2. Alternatives evaluated

### A. Status quo

Four ConfigMaps, VA CRD spec, annotations, env vars. Mature; tests already cover all paths.

| Strengths | Weaknesses |
|---|---|
| Zero migration cost. | Six surface layers; ~60 distinct knobs; no single source of truth. |
| Tests exist for every surface. | Validation timing is inconsistent (admission vs parse vs runtime). |
| Already works in production. | New analyzers (throughput, SLO) and new limiters (quota) will keep adding surfaces. |

**Verdict.** Defensible until ~3 surfaces. At 6 layers, the cumulative discoverability and validation cost outweighs the migration cost.

### B. Collapse the four ConfigMaps into one

A single `wva-policy-config` ConfigMap with sections for each concern. Parsed by a unified loader.

| Strengths | Weaknesses |
|---|---|
| Fewer ConfigMaps to find. | Still untyped at admission. |
| One Validate() call. | Still no per-namespace structure beyond the existing `default`-vs-`{modelID}#{namespace}` map-key convention. |
| Cheap to implement. | Mixes orthogonal concerns (analyzer policy vs scale-to-zero policy) in one file — large blast radius for any change. |

**Verdict.** Marginal improvement. Doesn't solve admission-time validation, doesn't introduce typed schema, doesn't help GitOps.

### C. Move all per-model policy to annotations on `ScaledObject`/`HPA` (flat Knative-style)

Push every per-model knob onto annotations alongside the discovery annotations.

```yaml
metadata:
  annotations:
    llm-d.ai/managed: "true"
    llm-d.ai/model-id: "ibm/granite-13b"
    llm-d.ai/saturation.scale-up-threshold: "0.90"
    llm-d.ai/saturation.scale-down-boundary: "0.65"
    llm-d.ai/saturation.priority: "2.0"
    llm-d.ai/scale-to-zero.retention: "10m"
    llm-d.ai/queueing-model.slo-multiplier: "4.0"
```

| Strengths | Weaknesses |
|---|---|
| Single surface per workload. | Annotations are stringly-typed; no OpenAPI validation. |
| Aligns with Knative's pattern. | Knative has 30+ such annotations; the UX is widely criticised for it. |
| GitOps-trivial — one file changes. | Cross-cutting concerns ("set priority=2 for all prod models") require touching every `ScaledObject`. |
| No new CRDs to install. | Couples policy to a specific scaling integration (KEDA's `ScaledObject`). |

**Verdict.** Works for low-knob counts; degrades as analyzer/quota/SLO configuration arrives. The per-namespace cross-cut weakness is load-bearing.

### D. Two scoped CRDs (`ClusterScalingPolicy` + `ScalingPolicy`)

```yaml
# Cluster-scoped defaults
kind: ClusterScalingPolicy
metadata:
  name: default
spec:
  saturation: { ... }
  scaleToZero: { ... }
---
# Namespace-scoped override
kind: ScalingPolicy
metadata:
  name: granite-13b
  namespace: production
spec:
  modelSelector:
    matchLabels:
      llm-d.ai/model-id: ibm/granite-13b
  saturation:
    scaleUpThreshold: 0.95
```

| Strengths | Weaknesses |
|---|---|
| OpenAPI validation on admission. | Two new CRDs to install, learn, and version. |
| Typed schema, `kubectl explain` works. | More objects to maintain in GitOps repos. |
| Cluster vs namespace scoping is K8s-native. | Selectors add a layer of indirection — "which policy applies?" requires evaluating selectors. |
| `kubectl get scalingpolicy -A` shows every policy. | Migration from ConfigMaps is non-trivial. |
| Single source of truth per scope. | The "select by model ID" approach has subtle precedence questions when two policies match. |

**Verdict.** Strong discoverability and validation story. Cost is real (more objects, more learning) but one-time.

### E. Hybrid: typed policy CRDs (D) + annotation overrides for hot knobs

Same as D, plus ~5 annotations on `ScaledObject`/`HPA` for the most-changed knobs (pause, scale-bounds, priority, cost, role).

| Strengths | Weaknesses |
|---|---|
| Heavy schema in typed CRDs; emergency knobs where operators reach. | Two surface layers — operators learn both exist. |
| Knative + KServe pattern. | Whitelist needs maintenance. |
| GitOps-friendly for steady-state; incident-friendly for overrides. | Two precedence rules to document. |
| Discovery and policy decoupled. | More to test. |

**Verdict.** Strong if compatibility with operator habits matters; carries permanent two-surface complexity to win one ergonomic property (faster emergency pause). The CRD's `spec.paused` field + a tight reconcile loop subsumes that ergonomic.

### F. `InferencePool` annotations (Gateway API inference extension)

```yaml
apiVersion: inference.networking.k8s.io/v1alpha1
kind: InferencePool
metadata:
  name: granite-13b-pool
  annotations:
    scaling.llm-d.ai/saturation.scale-up-threshold: "0.85"
    scaling.llm-d.ai/priority: "2.0"
```

| Strengths | Weaknesses |
|---|---|
| Aligns with the K8s ecosystem's direction for inference workloads. | `InferencePool` API is alpha; breaking changes likely. |
| "Model" is naturally a pool-shaped concept. | Couples WVA's policy UX to a specific upstream API. |
| Zero new WVA CRDs. | Inherits the annotation untyped-schema problem from C. |

**Verdict.** Worth reading from when present (additive — WVA reads `InferencePool` annotations if available). Not a substitute for typed CRDs.

### G. K8s `ResourceQuota` and `LimitRange` wholesale

| Strengths | Weaknesses |
|---|---|
| Zero new objects to learn. | `ResourceQuota` has no per-GPU-model granularity (see design-1002 §3.B). |
| K8s-native. | Cannot express analyzer thresholds, priorities, scale-to-zero retention, or anything WVA-specific. |

**Verdict.** Necessary for the quota limiter problem (the quota proposal already covers this), but insufficient as a *policy* UX. Strictly orthogonal.

### G′. Annotated `ResourceQuota` for per-GPU-type quotas

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: gpu-quota-h100
  namespace: production
  annotations:
    scaling.llm-d.ai/gpu-type: "H100"
spec:
  hard:
    requests.nvidia.com/gpu: "16"
```

K8s-native, GitOps-native, no new CRD. Fails on K8s admission semantics:

- The kube-apiserver does not interpret the annotation. It enforces each `ResourceQuota` independently against the device plugin's single `requests.nvidia.com/gpu` counter. With multiple `ResourceQuota`s on the same resource, admission is a logical AND — the effective cap becomes `min(hard_i)` across types. An admin intending "16 H100 + 4 A100" gets "4 total of any kind."
- `ResourceQuota.spec.scopeSelector` operands are limited to `PriorityClass` / `BestEffort` / `Terminating` / `CrossNamespacePodAffinity` — no per-pool scoping.
- The schema fragments across `N × T` `ResourceQuota` objects (N namespaces × T GPU types) instead of a single map.

Per-GPU-type quotas at admission require DRA (`<deviceclass>.deviceclass.resource.k8s.io/devices`), K8s 1.34+, with KEP #4840 still maturing.

**Verdict.** Rejected as a substitute. The correct composition is `ScalingPolicy.spec.quota` as the primary surface plus WVA reading `ResourceQuota` as a complementary input via design-1002's A2 (`ResourceQuotaReader`).

### H. Helm values as the policy surface

| Strengths | Weaknesses |
|---|---|
| Familiar to teams already using Helm. | Tightly couples deploy-time and runtime concerns. |
| One file diff per policy change. | Requires `helm upgrade` per policy change — runtime knobs become deploy-time knobs. |
| | Doesn't compose with operators who use Kustomize/Argo. |

**Verdict.** Wrong tool for runtime policy. Helm/Kustomize stay the install-time surface.

### S0. One cluster-scoped CRD with a `models[]` map (typed ConfigMap)

```yaml
kind: ScalingPolicy
metadata:
  name: cluster      # singleton
spec:
  default: { ... }
  overrides:
    - modelID: ibm/granite-13b
      namespace: production
      saturation: { scaleUpThreshold: 0.95 }
```

**Verdict.** Single object becomes a write bottleneck — every policy change is a parallel-PR merge conflict, single RBAC subject, single `kubectl describe` that grows with override count. Tempting because it mirrors today's ConfigMap; loses precisely the property we're trying to gain.

### S2. One cluster-scoped CRD with selectors + weights (Karpenter `NodePool` shape)

```yaml
kind: ScalingPolicy
spec:
  weight: 100
  namespaceSelector:
    matchLabels: { tier: production }
  modelSelector:
    matchLabels: { llm-d.ai/model-id: ibm/granite-13b }
  saturation: { ... }
```

**Verdict.** Necessary if WVA needed "all `tier=prod` namespaces inherit X" without enumerating namespaces. Most clusters have a handful of namespaces. The selector-and-weight engine carries permanent complexity (conflict resolution, status reporting) for a use case the simpler three-tier lookup handles via Kustomize patches.

### S1. One namespaced CRD, name-based three-tier lookup (RECOMMENDED)

The chosen design. See `design-config-ux.md`.

---

## 3. Comparison matrix

The matrix below compares each shortlisted alternative against the dimensions the proposal's goals call out.

| Property | A: status quo | B: one ConfigMap | C: flat annotations | D: two CRDs | E: D + annotations | S0: one cluster CRD (singleton) | S2: cluster CRD + selectors | **S1: namespaced CRD, 3-tier (chosen)** |
|---|---|---|---|---|---|---|---|---|
| Admission-time validation | ❌ (parse-time) | ❌ | ❌ | ✅ | ✅ schema; ❌ ann. | ✅ | ✅ | ✅ |
| Single source of truth | ❌ (6 layers) | partial | ✅ per workload | ✅ per scope | ✅ with documented precedence | ✅ (one object) | ✅ | ✅ |
| Number of kinds to learn | 4 ConfigMaps + CRD | 1 ConfigMap + CRD | 0 + CRD | 2 | 2 + annotation set | 1 | 1 | 1 |
| `kubectl explain` for policy | partial | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| K8s-native RBAC for governance | ❌ (ConfigMaps) | ❌ | ❌ | ✅ | ✅ | ❌ (single object) | ✅ | ✅ |
| Cross-cutting changes | ConfigMap edit | ConfigMap edit | touch every `ScaledObject` | one CRD edit | one CRD edit | one map edit (bottleneck) | one selector edit | one Kustomize patch |
| GitOps merge isolation | partial | partial | ✅ per object | ✅ per object | ✅ per object | ❌ (singleton conflicts) | ✅ per object | ✅ per object |
| Emergency pause | edit ConfigMap, wait | edit ConfigMap, wait | annotation edit | CRD edit | annotation edit | edit singleton, wait | CRD edit | CRD edit (`spec.paused`) |
| Per-GPU-type quota | ❌ (out of scope) | ❌ | ❌ | ✅ (in CRD) | ✅ | ✅ | ✅ | ✅ |
| Per-pool granularity (post-transition per-role) | ❌ | ❌ | partial (poolRef annotation) | partial | partial | partial | ✅ (selector) | ✅ (poolRef) |
| Composes with VA CRD deprecation | yes | yes | yes | yes | yes | yes | yes | yes |
| Composes with quota limiter (#1002) | bolt on a ConfigMap | section in unified ConfigMap | more annotations | first-class | first-class | first-class | first-class | first-class |
| Composes with throughput analyzer (#1052) | new ConfigMap fields | new ConfigMap section | more annotations | typed | typed | typed | typed | typed |
| Net new objects to install | 0 | 0 | 0 | 2 CRDs | 2 CRDs + annotations | 1 CRD | 1 CRD | 1 CRD |

S1 is selected on the basis of: K8s-native RBAC for the governance split, single kind to learn, no selector engine, no annotation surface to maintain, no singleton bottleneck. Each dimension where another option matches S1, S1 ties; the dimensions where S1 wins outright are kind count (1 vs 2 for D/E) and absence of the singleton bottleneck (S0) or selector machinery (S2).

---

## 4. Peer autoscaler patterns

| System | Primary surface | Cluster defaults | Per-workload override | Multi-tenancy |
|---|---|---|---|---|
| **Kubernetes HPA** | CRD spec only | none (kube-controller-manager flags only) | every HPA carries its full spec | namespace-scoped HPAs |
| **KEDA** | `ScaledObject` CRD + ~5 annotations | none | every `ScaledObject` carries its full spec | namespace-scoped + `ClusterTriggerAuthentication` |
| **Knative KPA** | `config-autoscaler` ConfigMap (cluster defaults) + per-Revision annotations (~30 annotations) | ConfigMap | annotations on revision template | namespace-scoped + KnativeServing CR for ops |
| **Kueue** | `ClusterQueue` (cluster) + `LocalQueue` (ns) + `ResourceFlavor` (cluster) + `Workload` (job-scoped) | `ClusterQueue` + `ResourceFlavor` | `LocalQueue` references `ClusterQueue`; per-`Workload` priority/preemption | cluster + namespace + cohort-based borrowing |
| **Karpenter** | `NodePool` (cluster) + `EC2NodeClass` (cluster) | `NodePool.spec.weight` priority | per-pod tolerations / `nodeSelector` consumed via Pod spec | NodePool selectors; no native namespace boundary |
| **KServe** | `InferenceService` CRD spec + Knative annotations | Knative `config-autoscaler` | per-`InferenceService` annotations | namespace-scoped + Knative defaults |
| **AWS Application Auto Scaling** | per-resource `ScalingPolicy` API objects (target-tracking) | none — every policy explicit | per-resource | account/region-scoped |
| **K8s ResourceQuota / LimitRange** | namespace-scoped CRDs | none | namespace overrides | namespace-scoped only |

### Patterns visible across peers

1. **Two layers is the dominant pattern.** Knative, KServe, and Kueue all run a cluster-defaulting layer plus a per-workload override layer. Single-layer designs (HPA, KEDA, AWS Application Auto Scaling) force per-workload boilerplate.
2. **Typed CRDs dominate for the heavy schema; annotations cover the hot knobs.** Knative is the closest analog to WVA's mix. The Knative community has had to actively *resist* growing the annotation list because each new annotation is a stringly-typed concept.
3. **Discovery and policy are usually separate surfaces.** KEDA uses `ScaledObject` for *both* discovery and scaler config — the design WVA's deprecation moves *toward*. Knative/KServe split discovery (`Service`/`InferenceService`) from policy (annotations + ConfigMap). The split shows up positively in retrospective analyses.

The 2024-2026 trend is consistent: **typed CRDs for the steady-state policy, annotations as a small escape valve, install-time config for infrastructure**. WVA's S1 design tracks that direction, with one departure: S1 uses three-tier name lookup instead of selectors, because WVA's policy granularity is namespace-shaped not cluster-shaped, so selectors are unnecessary complexity.

---

## 5. Design decisions captured during analysis

The proposal records the final design. The decisions below capture *why* — the rejected alternatives and the reasoning — so future reviewers don't relitigate them.

1. **Match key is `spec.poolRef`, not `spec.modelID`.** The model identifier is derived from the referenced `InferencePool` and surfaced as `status.modelID` (read-only). Operators never type the model string into a policy spec; they author `poolRef`. Earlier drafts used `spec.modelID` with a DNS-safe slug rule for `metadata.name`; rejected once we accepted llm-d-as-stack and could rely on `InferencePool` being available.

2. **Cluster default missing → controller refuses to start.** The cluster default is the only source of quota; falling back to hard-coded built-ins would hide quota policy behind invisible defaults.

3. **`spec.quota` in non-cluster-default policies is rejected at admission via CEL, not silently ignored.** Silent ignores would let namespace owners set quota fields thinking they apply.

4. **No `perNamespace[ns].exclude` field.** The existing namespace annotation `wva.llmd.ai/exclude` already opts a whole namespace out.

5. **`status.effectivePolicy` lives on the workload (`ScaledObject`/`HPA`), not on the `ScalingPolicy`.** Operators describe workloads when debugging.

6. **No `spec.role` or `spec.perRole` field.** Per-pool keying inherits per-role semantics for free once per-model-per-role `InferencePool`s become the norm.

7. **No per-role quotas.** Cluster admins set namespace caps; the engine allocates within. Role differentiation is expressed via per-pool priority and `scaleBounds`.

8. **CEL list-uniqueness rejects duplicate `spec.poolRef` at admission.** Feasible because the llm-d stack's K8s floor (1.32+ via the inference extension's "last three minors" policy) is well above CEL's 1.30 GA threshold.

9. **List merge for `spec.analyzers[*]` is by `name` field, not replace-wholesale.** Operators tune one analyzer at a time without inheriting responsibility for redeclaring others.

10. **Delegated quota editing via `quota-editor` `ClusterRole`, not a separate CRD.** Expressible in standard K8s RBAC by scoping `update`/`patch` to `resourceNames: ["default"]`.
