# Proposal: ScalingPolicy CRD for WVA configuration

---

## Problem Statement

WVA configuration is spread across six layers: four ConfigMaps (`wva-variantautoscaling-config`, `wva-saturation-scaling-config`, `wva-queueing-model-config`, `wva-model-scale-to-zero-config`), the `VariantAutoscaling` CRD spec, annotations on `ScaledObject`/`HPA`/Namespace, ~25 environment variables, Kustomize overlays, and implicit surfaces like hardcoded `ServiceMonitor` names — roughly 60 knobs across six locations.

These knobs are **heterogeneous and rightly live in different places**: some are per-controller and install-time (e.g. Prometheus base URL — never overridden at a finer grain), others are **per-pool scaling policy** that's frequently tuned (e.g. the KV-cache threshold — a strong desire to override it per pool). This proposal does **not** seek to unify all 60 or give them one override path. The problem it targets is narrower: the **scaling-policy** knobs — roughly 15–20 parameters (saturation thresholds, analyzer selection and weights, scale-to-zero, queueing-model SLO targets, scale bounds), not the full 60 — are themselves split across three of those ConfigMaps (`wva-saturation-scaling-config`, `wva-queueing-model-config`, `wva-model-scale-to-zero-config`), the VA spec, and annotations, with inconsistent override semantics and validation. The rest (infrastructure env vars/flags, discovery) stay where they are.

An operator asking "what policy applies to model `granite-13b` in namespace `prod`?" must inspect a global ConfigMap, a possible namespace-local override of the same ConfigMap, a separate scale-to-zero ConfigMap, the queueing-model ConfigMap, annotations on the `ScaledObject`, fields on the VA CRD, and environment variables on the controller. No single `kubectl` command returns the effective policy.

Validation is a gap today, but not a differentiator. The policy ConfigMaps and annotations currently have no admission validation, so a bad threshold or a typo'd key is caught late — in controller logs at runtime, or (for some fields) silently defaulted, never rejected at `kubectl apply`. That gap is closeable either way: ConfigMaps and annotations *can* be admission-validated with a `ValidatingAdmissionPolicy` (GA in K8s 1.30, CEL, no webhook). The CRD's actual edge is **structure**, not validation — typed and defaulted fields, `kubectl explain` discoverability, OpenAPI + CEL co-located on the schema — plus collapsing the scattered policy surfaces into one (a `ValidatingAdmissionPolicy` closes the validation gap but leaves the sprawl).

Anticipated growth makes this worse: absent a deliberate shape, each new feature tends to land as **yet another ConfigMap** — a genuinely new surface (quota, #1002, is next in line) — and that is the trend this proposal interrupts. Adding *keys* (ConfigMap fields or annotations) doesn't add a surface; a new ConfigMap does. (PR #1052, for instance, added per-analyzer thresholds to the *existing* saturation ConfigMap — more knobs, same surface.)

This proposal introduces a single typed CRD as the source of truth for scaling policy, while leaving discovery on annotations and infrastructure config on env vars / Kustomize. Three configuration domains, three surfaces, no overlap.

The day-1 value stands on its own, independent of quota (deferred to #1162): the three policy ConfigMaps collapse into one typed `ScalingPolicy` CRD — admission-validated (OpenAPI + CEL), discoverable via `kubectl explain`, resolved through a single three-tier precedence — replacing scattered, parse-time-validated maps with one inspectable surface.

---

## Design Philosophy

Three configuration domains across three surfaces — discovery annotations, the `ScalingPolicy` CRD, and infrastructure (env vars / Kustomize). Each surface has one purpose and one precedence rule; on the `ScalingPolicy` CRD the resolution *tier* decides the owner (tenant at the namespace/pool tiers, cluster admin at the cluster default). (Domains — *what kind* of config — are distinct from the resolution tiers introduced below.)

### Personas

This proposal uses two personas consistently:

- **Cluster admin** — operates the WVA install and the system namespace; owns platform-wide defaults and quota.
- **Tenant** — owns a namespace and the model workloads deployed in it; tunes scaling policy for their own pools but cannot change quota. In a Model-as-a-Service platform the tenant is the serving/platform team that deploys the model servers — not the end users who consume the API.

Elsewhere in this doc, "operator" means whoever is acting in context — usually the *tenant*.

This split assumes a shared, cluster-scoped install. In a namespace-scoped install (one WVA instance per namespace) the two personas collapse: that tenant owns every surface for their own namespace. **Quota is the exception** — it is intrinsically cluster-scoped and cannot be owned or enforced per namespace (see [Quota & Deployment Scope](#quota--deployment-scope)).

| Configuration domain | Surface | Owner |
|---|---|---|
| Discovery (which workloads WVA manages) and per-workload identity | Annotations on `ScaledObject`/`HPA` | Tenant |
| Scaling policy: thresholds, analyzers, scale-to-zero, priority | `ScalingPolicy` — per-pool & namespace tiers (the only new surface) | Tenant |
| Cluster-wide policy: limiters, optimizer, quota (quota → #1162) | `ScalingPolicy` — cluster-default tier | Cluster admin |
| Infrastructure: Prometheus URL, TLS, leader election, log level | Env vars + Kustomize overlays | Cluster admin (install-time) |

Discovery and per-pool/namespace policy are tenant-owned because each tenant knows their own workloads. The cluster-default tier — limiters, optimizer, quota — is cluster-admin-owned because those are platform-wide. It is the **same `ScalingPolicy` CRD**; the tier decides the owner, and namespace RBAC enforces the split (cluster admins write the system namespace, tenants write their own). Infrastructure is install-time because it doesn't change per-workload.

**Single configuration surface.** The aim is that `ScalingPolicy` is the *one* place a user configures WVA scaling. The only WVA-specific things left on the `ScaledObject`/`HPA` are the discovery **label** (`llm-d.ai/managed`, a label not an annotation — per #1130) and the few per-workload annotations that aren't derivable (`variant-cost`, `role`) — not policy. New configuration, including quota's eventual surface, belongs in `ScalingPolicy`, not in another ConfigMap.

---

## Goals

1. A single **authoring** source of truth for scaling policy — one place to define it, instead of three policy ConfigMaps + the VA spec + annotations
2. A single **inspectable effective policy** for any workload — one command returns what's in force (the precedence rule and tooling are described in Proposed Solution)
3. Config errors are caught at `kubectl apply`, not in controller logs at runtime — fields are natively typed and admission-validated (OpenAPI + CEL on the schema), not opaque strings; extensible analyzer/limiter parameters validate at load (see [Schema](#schema-scaling-engine-configuration))
4. A tenant cannot change another namespace's policy (nor, once quota lands per #1162, raise their own GPU cap) — the governance split is enforced by native K8s RBAC, with no custom admission code
5. New analyzers and limiters (e.g. the throughput analyzer, #1052) compose without a schema redesign

## Non-Goals

- Replacing infrastructure config (Prometheus URL, TLS, leader election) — those stay in env vars / Kustomize
- Replacing the discovery annotations on `ScaledObject`/`HPA` — those stay
- Removing ConfigMaps in one shot — a non-breaking phased rollout is part of the plan
- A single override or validation mechanism for *all* config — the 60 knobs are heterogeneous (per-controller vs per-pool), so each domain keeps its appropriate surface and validation: CRD admission for policy, startup/env validation for infrastructure. The aim is one surface *per concern*, not one surface for everything
- Multi-cluster federation, custom analyzer authoring UX, observability tuning — out of scope

---

## Proposed Solution

### One Namespaced CRD: `ScalingPolicy`

A single namespaced CRD under `scaling.llm-d.ai/v1alpha1` carries every scaling-policy setting. The match key is `spec.inferencePoolRef` — a `LocalObjectReference` to the `InferencePool` (defined by the Gateway API Inference Extension, `gateway-api-inference-extension`) whose pods the policy applies to. The policy points at the pool, so the workload's `ScaledObject`/`HPA` carries no WVA-specific reference — consistent with the single-surface goal.

```yaml
apiVersion: scaling.llm-d.ai/v1alpha1
kind: ScalingPolicy
metadata:
  name: granite-premium-priority      # name describes purpose, not identity
  namespace: production
spec:
  inferencePoolRef:
    name: granite-premium-pool        # InferencePool in the same namespace
  priority: 2.0
  scaleToZero:
    enabled: false
  analyzers:
    - type: saturation
      parameters:
        scaleUpThreshold: 0.95
```

The WVA controller discovers which `InferencePool` a workload's pods belong to by watching `InferencePool` objects (from the Gateway API Inference Extension, `gateway-api-inference-extension`, which defines `InferencePool`) and matching their `spec.selector` against the workload's pod template labels. Tenants do not author the pool-to-workload mapping; the controller derives it from the inference-extension API.

### Schema: Scaling-Engine Configuration

WVA's scaling engine has pluggable **analyzers** (saturation V1/V2, queueing-model, throughput) and **limiters** (e.g. physical GPU inventory, quota), plus a single **optimizer** stage. The pluggable parts must let a new analyzer or limiter plug in **without changing the CRD**, so they reuse the plugin pattern of **EPP** — the EndPoint Picker, llm-d's inference scheduler from the Gateway API Inference Extension — whose `EndpointPickerConfig` configures plugins as a name-keyed list of `{ type, name, parameters }`, where `parameters` is plugin-specific and **not** constrained by the top-level schema (`x-kubernetes-preserve-unknown-fields`); each plugin parses and validates its own `parameters` at load.

```yaml
spec:
  # tenant-tunable (any tier) — typed cross-cutting fields + analyzers
  priority: 2.0                  # typed, OpenAPI/CEL-validated, kubectl explain
  scaleToZero: { enabled: false }
  analyzers:                     # one or more — {type, name, parameters}
    - type: saturation
      parameters: { scaleUpThreshold: 0.95 }
  # instance-wide (cluster-default tier only)
  limiters:                      # zero or more — compose as a chain
    - type: gpu-inventory
    - type: quota                # parameters schema in #1162
```

**Cardinality and tier.**
- **`analyzers`** — a pluggable list (one or more), tenant-tunable at any tier.
- **`limiters`** — a pluggable list that composes as a chain (e.g. physical GPU inventory + quota); instance-wide, so it lives on the **cluster-default** `ScalingPolicy` (a CEL rule keeps it off namespace/per-pool policies, the same way `spec.quota` is cluster-default-only).
- **optimizer** — a single, **fixed** stage; not selected and not exposed in the schema today. Its behavior is internal: cost-minimizing when unconstrained, fair-sharing by `priority` when a limiter binds (the cost-aware/greedy-by-score split is an implementation detail, not a user choice). There is nothing to configure on it now; if real knobs emerge later, a `spec.optimizer` block can be added with no redesign.

Per-pool and namespace policies carry the tenant-tunable fields (analyzer thresholds, `priority`, `scaleToZero`).

Two tiers of validation, deliberately:
- **Stable, cross-analyzer fields** (`priority`, `scaleToZero`) are typed — OpenAPI/CEL-validated and discoverable via `kubectl explain`.
- **Plugin `parameters`** are `x-kubernetes-preserve-unknown-fields`; the analyzer/limiter validates them at load. This is the same trade-off EPP makes — extensibility at the cost of admission-time validation of the inner fields. A new analyzer or limiter ships its own `parameters` keys and the CRD schema never changes.

**EPP / WVA boundary.** This reuses EPP's *shape*, not its config. WVA and EPP share only the `InferencePool` as a common object — EPP owns scheduling/plugin config (`EndpointPickerConfig`), WVA owns scaling policy, and both merely *read* the pool. There is no two-way config to keep in sync.

### Three-Tier Resolution

For each workload, the controller resolves a policy via three deterministic lookups, in fixed order. These three are the **only** resolution tiers — there is no per-variant tier; variant cost/hardware/capacity differences are resolved by the optimizer, not by policy (see [Per-Pool Keying and Roles](#per-pool-keying-and-roles)):

1. **Per-pool override** — `ScalingPolicy` in the workload's namespace with `spec.inferencePoolRef.name` matching the workload's pool.
2. **Namespace default** — `ScalingPolicy` in the workload's namespace with `spec.inferencePoolRef` absent.
3. **Cluster default** — `ScalingPolicy` in the system namespace (default `workload-variant-autoscaler-system`, configurable via `--system-namespace`) with `spec.inferencePoolRef` absent.

Fields merge from cluster default → namespace default → per-pool override. Scalars: higher tier wins if set. Maps: overlay per key. Lists with a natural identifier (`spec.analyzers[*]` keyed by `name`): merge by name — lower-tier entries inherited unless the higher tier overrides them, higher-tier entries added when no lower-tier match exists. Lists without an identifier: replace wholesale. (`limiters` and `optimizer` live only on the cluster default, so they aren't merged across tiers.)

The namespace and cluster defaults double as **shareable scaling profiles**: common config defined once there applies across all pools in scope, and per-pool policies carry only the deltas — so sharing policy across pools needs no per-workload reference (an `HPA → ScalingPolicy` ref would instead put a WVA-specific field back on every workload). Sharing across an arbitrary *subset* of pools that doesn't align with namespace/cluster boundaries is the one case the tiers don't cover; if it's ever needed, a `labelSelector` on the `ScalingPolicy` (still policy→pools) is the extension — see Alternatives.

The effective policy is a property of the **pool**, not of each variant or workload — WVA scales the pool, and the optimizer distributes replicas across the pool's variants (see [Per-Pool Keying and Roles](#per-pool-keying-and-roles)). WVA stays **read-only** on the `ScaledObject`/`HPA`: it never writes back to tenant-owned objects. The merged result is surfaced on demand by `kubectl get scalingpolicy` and `wva-config explain <pool>` (a proposed WVA CLI; see Migration), which pretty-prints the merged policy and its three sources — no manual traversal of the `HPA → … → ScalingPolicy` chain. The per-variant output remains the `wva_desired_replicas` metric, unchanged.

### Quota: Single Surface, Schema in #1162

Quota is a required feature, but its `spec.quota` schema is settled in the dedicated quota proposal (#1162, for issue #1002) — not here. The shared position is firm: quota is declared **directly on the cluster-default `ScalingPolicy`**, with no interim standalone quota ConfigMap — shipping one would reintroduce exactly the fragmentation this proposal removes. Quota is a scaling *constraint* (the GPU budget the optimizer allocates within), so it belongs on this single surface; #1162 owns its schema and the cluster-scoped enforcement.

#### Quota & Deployment Scope

Quota is intrinsically **cluster-scoped**, for two reasons:

- **No global arbiter per namespace.** A namespace-scoped WVA instance sees only its own namespace, so it cannot enforce an *overall cluster* cap. The sum of per-namespace caps can exceed real cluster GPU capacity, and independent per-namespace instances each see the full cluster inventory (via node discovery) without coordinating — so they would collectively overcommit. Enforcing a cluster cap requires one controller with a cluster-wide view.
- **Ownership.** Per-namespace caps are a cluster-admin decision; a tenant must not be able to raise their own. A per-namespace deployment would put the quota config in the tenant's hands.

So quota must live in a cluster-admin-owned, system-namespace surface enforced by a cluster-scoped controller. Namespace-scoped WVA remains valid for thresholds/scale-to-zero (tenant-owned, per namespace); it just cannot own or enforce quota.

### Per-Pool Keying and Roles

WVA scales the **pool**, not individual variants: the analyzer produces pool-level capacity signals and the optimizer distributes replicas across the pool's variants by cost-efficiency, capacity, and accelerator type. Variant differences — cheap vs expensive, throughput-optimized, different hardware — are *optimizer inputs*, not policy axes; they need correct cost/capacity numbers (which WVA already reads), not separate policies. So there is **one `ScalingPolicy` per pool**, matched by `spec.inferencePoolRef` — no per-variant tier and no top-level `spec.role`.

**Roles (prefill / decode).** The planned direction is a separate `InferencePool` per role; once that lands, one `ScalingPolicy` per (role-)pool gives per-role policy for free, with nothing extra. Until then, P/D disaggregation ships in KServe today as a **single `InferencePool` with two independently scaled workloads** (prefill, compute-bound; decode, memory-bandwidth-bound) that need different saturation thresholds. Those are **analyzer knobs** — the v2 saturation analyzer already computes per-role — so per-role differences are expressed inside the analyzer's `parameters`, not as a second policy object or a top-level field:

```yaml
spec:
  inferencePoolRef:
    name: granite-premium-pool
  analyzers:
    - type: saturation
      parameters:
        perRole:
          prefill: { scaleUpThreshold: 0.95 }
          decode:  { scaleUpThreshold: 0.92 }
```

One policy per pool; the analyzer applies the per-role values. When per-role `InferencePool`s arrive, the `perRole` block collapses into plain per-pool config with no migration. (`perRole` is analyzer-owned config under the extensible schema above, so it needs no CRD change.)

### Conflict Detection and Bootstrap

Two `ScalingPolicy` objects in the same namespace cannot target the same `spec.inferencePoolRef.name` — a pool carries at most one policy. A CEL `x-kubernetes-validations` uniqueness rule on `inferencePoolRef.name` rejects duplicates at admission. CEL `x-kubernetes-validations` went GA in K8s 1.29; the lowest K8s version supported elsewhere in the llm-d stack (1.32+ via the inference extension's "last three minors" policy) is well above that floor, so no controller-side fallback or admission webhook is needed.

The cluster-default `ScalingPolicy` is **optional**. When absent, threshold/scale-to-zero resolution falls through to built-in defaults — there is no refuse-to-start. (When quota is later introduced, the cluster default becomes required *for installs that use quota*, and only then must be cluster-admin-owned and enforced by a cluster-scoped controller — see [Quota & Deployment Scope](#quota--deployment-scope).) Namespace defaults and per-pool overrides remain optional and fall through to the next tier normally when absent.

### Status Conditions

`ScalingPolicy.status` carries conditions about the policy object itself (not per-workload state):

- **`Accepted`** — schema + CEL admission passed.
- **`PoolResolved`** — `spec.inferencePoolRef` matched an `InferencePool` and `status.modelID` was derived; `False` with reason `PoolNotFound` otherwise.
- **`InEffect`** — the policy is the resolved tier for at least one pool (`Superseded` otherwise).

Duplicate `spec.inferencePoolRef.name` policies are rejected at admission by the uniqueness CEL rule, so they never reach status.

---

## What Does NOT Change

- Discovery via the `llm-d.ai/managed` **label** on `ScaledObject`/`HPA` (a label, not an annotation — see #1130)
- Per-workload identity annotations (`llm-d.ai/variant-cost`, `llm-d.ai/role`)
- Namespace-level controls (`wva.llmd.ai/config-enabled`, `wva.llmd.ai/exclude`)
- Infrastructure configuration (Prometheus URL, TLS, leader election, log level — still in env vars + Kustomize overlays)
- vLLM/EPP metric collection and Prometheus queries
- Analyzer pipeline (saturation V1/V2, queueing model, throughput)
- Optimizer (cost-aware, greedy-by-score)
- `wva_desired_replicas` exposure and KEDA/HPA consumption

---

## Relationship to the VA CRD Deprecation

This proposal owns *policy*; the VA CRD deprecation (#1092) owns *discovery* and per-workload identity. Two consequences worth stating explicitly:

- **`model-id` is proposed for removal.** `status.modelID` is derived from the referenced `InferencePool`, so the per-workload `llm-d.ai/model-id` annotation is redundant — WVA reads the pool instead. That shrinks the workload's footprint to the discovery label (`llm-d.ai/managed`) plus the annotations that aren't derivable (e.g. `variant-cost`).
- **Per-role policy collapses into per-pool.** The planned direction is a separate `InferencePool` per role; once it lands, one `ScalingPolicy` per pool gives per-role policy with no extra mechanism. Until then, per-role differences live in the analyzer's `parameters` (see [Per-Pool Keying and Roles](#per-pool-keying-and-roles)).

---

## Migration Path

### What Changes for Users

| Before | After |
|---|---|
| 3 policy ConfigMaps (saturation, queueing-model, scale-to-zero) | 1+ `ScalingPolicy` objects |
| Per-model overrides via `{modelID}#{namespace}` map keys | Per-pool `ScalingPolicy` objects, matched by `spec.inferencePoolRef` |
| Threshold typos surface in controller logs | Threshold typos rejected at `kubectl apply` |
| Quota planned as a separate ConfigMap (#1002) | Deferred to the dedicated quota proposal (#1162) |
| Discoverability: read three policy ConfigMaps | `kubectl get scalingpolicy` / `wva-config explain <pool>` |

### Migration Tool

A `wva-config migrate` CLI subcommand reads existing ConfigMaps and emits equivalent `ScalingPolicy` YAML:

```bash
wva-config migrate --dry-run
wva-config migrate --apply
```

The tool maps the cluster-default ConfigMap entries to a system-namespace `ScalingPolicy default` and per-model overrides to per-pool policies in the corresponding namespaces. (Quota migration is owned by the dedicated quota proposal.)

---

## Implementation Phases

### Phase 1: ScalingPolicy CRD alongside ConfigMaps (Non-Breaking)

**Goal:** Introduce the CRD and resolver; ConfigMaps continue to work unchanged.

**Deliverables:**
- `ScalingPolicy` CRD under `scaling.llm-d.ai/v1alpha1` with the full schema covering today's three policy ConfigMaps
- CEL validation rules: `spec.inferencePoolRef.name` uniqueness within a namespace; `limiters`/`optimizer`/`quota` permitted only on the cluster default
- `PolicyResolver` performing the three-tier lookup; controller reads from CRDs if present, falls back to ConfigMaps otherwise
- Per-pool effective-policy resolution surfaced via `kubectl get scalingpolicy` and a `wva-config explain <pool>` command (no write-back to `ScaledObject`/`HPA`)
- `--system-namespace` controller flag (default `workload-variant-autoscaler-system`)

**Success Criteria:** Either ConfigMap-only or CRD-only configurations produce identical scaling decisions; mixed mode resolves cleanly via the documented precedence.

### Phase 2: Deprecation

**Goal:** Mark ConfigMaps deprecated; default install ships CRDs.

**Deliverables:**
- `wva-config migrate` CLI tool
- Default samples in `config/samples/` use the CRD
- Deprecation warning event emitted when WVA reads from a ConfigMap
- `docs/user-guide/scaling-policy.md` describes the three-tier model (quota governance lands with #1162)

**Success Criteria:** All sample and documentation paths use the CRD; CI tests pass with ConfigMaps absent.

### Phase 3: ConfigMap Removal

**Deliverables:**
- Remove ConfigMap loader code from `internal/config/`
- Remove ConfigMap reconciliation from `internal/controller/`
- Retain analyzer, optimizer, collector, and engine code (unchanged)

**Success Criteria:** WVA binary no longer reads scaling-policy ConfigMaps. All functionality available via `ScalingPolicy`.

---

## Alternatives Considered

1. **Status quo (4 ConfigMaps + CRD spec + annotations + env vars + Kustomize).** Defensible until the surface count exceeds operators' working memory. At six layers, the cumulative discoverability cost outweighs the migration cost.

2. **Collapse the three policy ConfigMaps into one.** Cheap. Doesn't address validation timing or schema discoverability. Strictly worse than this proposal on every dimension except "ConfigMaps already exist."

3. **Keep the ConfigMaps and add a `ValidatingAdmissionPolicy` for validation.** Closes the admission-validation gap without a new CRD — `ValidatingAdmissionPolicy` (GA in K8s 1.30) enforces CEL rules on ConfigMap `data` and annotations in-process, no webhook. But it leaves every other problem untouched: values stay opaque strings (no typing, no defaulting, no `kubectl explain`); the rules live in separate policy/binding objects that re-encode the ConfigMap shape by hand; and the six-surface sprawl and discoverability gap — the *real* problem — are unchanged. It buys admission timing at the cost of maintaining a parallel, untyped schema in CEL.

4. **All per-pool policy on `ScaledObject`/`HPA` annotations (Knative-style).** Works for low knob counts; degrades as analyzer thresholds, quota, and SLO config arrive. Cross-cutting changes ("priority=2 for all prod models") force per-workload edits. The Knative community's well-documented regret about its 30+ autoscaling annotations is the cautionary case study.

5. **Two scoped CRDs (`ClusterScalingPolicy` + `ScalingPolicy`).** Forces tenants to decide "is this a cluster fact or a namespace fact?" up front. The single-CRD design defers that decision into the tier the policy sits in. Adding scope is a one-line edit, not a "delete here, recreate there" migration.

6. **One cluster-scoped CRD with `namespaceSelector` + `weight` (Karpenter `NodePool` shape).** Necessary if WVA needed "all `tier=prod` namespaces inherit X" without enumerating namespaces. Most clusters have a handful of namespaces, not hundreds. The selector-and-weight engine carries permanent complexity (conflict resolution, status reporting) that the three-tier name lookup avoids.

7. **Annotated `ResourceQuota` for per-GPU-type quotas.** Tempting (K8s-native, no new CRD), but K8s admission does not interpret the annotation; multiple `ResourceQuota`s on `requests.nvidia.com/gpu` compound as `min(hard_i)`, not per-type, and per-type caps at admission need DRA (K8s 1.34+). The full quota-surface comparison (including core-`ResourceQuota`-vs-`spec.quota`) is deferred to the dedicated quota proposal (#1162).

8. **`spec.modelID` as the match key (with `metadata.name` as a sanitized DNS-safe slug).** Earlier draft of this proposal. Rejected once we accepted llm-d-as-stack: the `InferencePool` reference is always available, exact-match, and post-transition naturally encodes role. `status.modelID` is preferable to `spec.modelID` because the model identity is derivable, not an authoritative input.

9. **Deploy-tool values (Kustomize overlays / Helm) as the policy surface.** Tightly couples deploy-time and runtime; requires a redeploy per policy change.

See `design-config-ux-analysis.md` for the full strengths/weaknesses breakdown of each alternative.

---

## Comparison Matrix

S1 (this proposal) compared with the alternatives above. Each cell summarises the option's behavior on a property the proposal's goals call out; `design-config-ux-analysis.md` carries the extended matrix with more dimensions.

| Property | A: status quo | B: one ConfigMap | C: flat annotations | D: two CRDs | E: D + annotations | S2: cluster CRD + selectors | **S1: this proposal** |
|---|---|---|---|---|---|---|---|
| Native typed + validated schema | ❌ | ❌ | ❌ | ✅ | partial | ✅ | ✅ |
| Single source of truth | ❌ (6 layers) | partial | ✅ per workload | ✅ per scope | ✅ with precedence | ✅ | ✅ |
| Kinds to learn for policy | 3 ConfigMaps | 1 ConfigMap | 0 (annotations only) | 2 CRDs | 2 CRDs + annotations | 1 CRD | 1 CRD |
| `kubectl explain` for policy | partial | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| K8s-native RBAC governance split | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| Cross-cutting changes | ConfigMap edit | ConfigMap edit | every `ScaledObject` | one CRD edit | one CRD edit | one selector edit | one Kustomize patch |
| Per-GPU-type quota | ❌ | ❌ | ❌ | ✅ | ✅ | ✅ | ✅ |
| Per-pool granularity | ❌ | ❌ | partial | partial | partial | ✅ (selector) | ✅ (`inferencePoolRef`) |
| Selector engine complexity | n/a | n/a | n/a | n/a | n/a | permanent | none |
| Net new objects to install | 0 | 0 | 0 | 2 CRDs | 2 CRDs + annotations | 1 CRD | 1 CRD |

S1 wins on the dimensions that the proposal's goals call out — admission-time validation, K8s-native RBAC, single kind to learn — while avoiding the selector engine S2 carries and the singleton bottleneck a Karpenter-style single-object design would impose. Where another option ties on a row, S1 ties; where S1 wins, it's on kind count, governance ergonomics, or absence of a separate annotation surface.

> The **Per-GPU-type quota** row reflects S1's *capability* to host quota first-class, not current scope: quota's configuration surface is deferred to the dedicated quota proposal (#1162).

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
3. **Discovery and policy are separate surfaces.** KEDA uses `ScaledObject` for both discovery and scaler config; Knative/KServe split discovery (`Service`/`InferenceService`) from policy (annotations + ConfigMap). The split shows up positively in retrospective analyses.

The 2024-2026 direction is consistent: **typed CRDs for steady-state policy, annotations as a small escape valve, install-time config for infrastructure**. WVA's design tracks that direction with one departure — name-based three-tier lookup instead of selectors, because WVA's policy granularity is namespace-shaped not cluster-shaped, so selectors are unnecessary complexity.
