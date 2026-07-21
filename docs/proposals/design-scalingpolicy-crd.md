# Proposal: WVA scaling configuration — schema and resolution (ConfigMap first, CRD later)

## Problem — what this addresses

Today WVA is configured by **one cluster-wide ConfigMap** plus **per-workload labels/annotations** on the KEDA `ScaledObject` (`llm-d.ai/managed`, `llm-d.ai/model-id`, `inference.optimization/acceleratorName`). That is enough for a single global configuration, but it cannot express:

- **per-namespace or per-pool policy** — every workload shares one global `priority`, `scaleToZero`, and analyzer configuration;
- **an ownership boundary** — cluster-admin-owned enforcement (GPU limiters / quota) and tenant-owned tuning (thresholds, priority) live in the same undifferentiated blob, with no way to give them different RBAC;
- **validation** — a typo in the ConfigMap is silently ignored;
- **a resolved-configuration surface** — an operator cannot see *what policy WVA actually applied* to a given pool.

This proposal fixes the configuration **schema** and **resolution** (the fields, the scope tiers, the pluggable-plugin envelope, and the merge order). It is **transport-neutral** and lands in two phases:

- **Phase 1 — extend the existing ConfigMap** with this schema. No CRD, no migration, no new install step. Validation via `ValidatingAdmissionPolicy` (CEL) + the controller; the ownership boundary via RBAC. This is the minimal core.
- **Phase 2 — promote the schema to a `ScalingPolicy` CRD** when field-level typed validation, `kubectl explain`, and a native `.status` are worth the operational cost of a CRD (§6). Same schema, same resolution — a delivery upgrade, not a redesign.

It does **not** change how WVA *actuates* (it still emits `wva_desired_replicas` for a `ScaledObject`, §3) or how it *identifies* the workloads it manages (§7).

## Optional and backward-compatible

The structured configuration is **optional and additive** — a workload with only the managed labels keeps scaling under WVA's defaults exactly as today. Two things to be accurate about, because the value here is **unification**, not "adding tiers":

- **WVA already tiers per-model/namespace.** The `saturation-scaling-config` ConfigMap already carries a `default` entry **plus per-model/namespace override entries** (each carrying `priority` and per-analyzer `score` and thresholds), and `scaleToZero` and the queueing-model analyzer are already namespace-aware in the controller config. So per-namespace scoping is **not new**.
- **What *is* new:** one **unified schema** across those currently-separate surfaces; the **per-pool** tier (finer than per-model/namespace); the admin/tenant **ownership boundary** (§2); and the typed **`behavior`** field (§4). `priority` (per-model), `scaleToZero`, thresholds and `enableLimiter` keep their current meaning.

Upgrading needs **no value migration**, but be honest that it *is* a **consolidation** — three config surfaces fold into one — not a pure restructuring of a single ConfigMap.

**Before / after.** Today's real `saturation-scaling-config` (abbreviated):

```yaml
metadata:
  name: saturation-scaling-config
  labels: { app.kubernetes.io/name: workload-variant-autoscaler }   # controller cache filters on this
data:
  default: |                         # global defaults
    scaleUpThreshold: 0.85
    priority: 1.0                    # per-model multiplier
    analyzers: [ { name: saturation, score: 1.0 } ]   # per-analyzer weight
    enableLimiter: false
  premium-override: |                # a per-(model, namespace) override entry
    model_id: granite-premium
    namespace: production
    priority: 2.0
```

Phase 1 keeps the same values in the unified schema, resolved by tier/placement (§2) instead of in-data `model_id`/`namespace` keys:

```yaml
# cluster-default tier == the 'default' entry above
priority: 1.0
scaleToZero: { enabled: false }
analyzers: [ { type: saturation, name: saturation, score: 1.0, parameters: { scaleUpThreshold: 0.85 } } ]
limiters:  [ { type: gpu-inventory } ]              # admin-only, cluster tier (§2)

# per-pool tier for granite-premium (== the override above), only the delta:
#   inferencePoolRef: { name: granite-premium-pool }
#   priority: 2.0
```

**Minimal usage.** A pool with no config of its own scales under the cluster default (unchanged). To raise one pool's priority, a tenant adds *just* the per-pool object carrying the delta (`inferencePoolRef` + `priority: 2.0`); everything else (`scaleToZero`, `behavior`, `analyzers`) inherits from the tiers above (§2).

## 1. The configuration object and how it is matched

The configuration is **one schema** carried by one object per scope:

- **Phase 1:** a ConfigMap whose `data` holds the schema as a YAML document (one per tier, §2).
- **Phase 2:** a namespaced `ScalingPolicy` custom resource (`scaling.llm-d.ai/v1alpha1`).

It is matched to a workload through that workload's **`InferencePool`** (the Gateway API Inference Extension object; the workload→pool link is the pool's pod selector) via `inferencePoolRef` — **not** through the workload and **not** through its `ScaledObject`. The config points at the pool; the pool's workloads carry no reference back.

```yaml
# Phase 2 (CRD) form — the phase-1 ConfigMap carries the same document under data:
apiVersion: scaling.llm-d.ai/v1alpha1
kind: ScalingPolicy
metadata: { name: granite-premium-priority, namespace: production }
spec:
  inferencePoolRef: { name: granite-premium-pool }   # the pool this config governs (per-pool tier)
  priority: 2.0                                       # per-model; see §2
  scaleToZero: { enabled: false }                     # per-model
  behavior:                                           # per-model; autoscaling/v2 HPA behavior (§4)
    scaleDown: { stabilizationWindowSeconds: 300 }
  analyzers:
    - type: saturation
      parameters: { scaleUpThreshold: 0.95 }          # plugin-owned; see §4
```

The tunable field families and their meaning, defined here so nothing below is referenced before it is defined:

- **`priority`** *(per-model)* — the model's weight when models contend for a shared GPU budget. It is only meaningful **relative to other models drawing on the same budget**: within a namespace quota if one exists, otherwise the shared cluster budget (so priority *does* compose across un-quota'd namespaces on the same accelerator type — how it does is engine/rescale territory, not this schema). It is not comparable across *independent* budgets. Default `1.0`.
- **`scaleToZero`** *(per-model)* — whether WVA may emit a desired count of `0` for an idle model (§3).
- **`behavior`** *(per-model, tenant-tunable)* — the HPA-style stabilization behavior WVA applies to its own recommendation (trailing windows, per-period rate policies, tolerance) before emitting it. This is a **typed** field reusing `k8s.io/api/autoscaling/v2` `HorizontalPodAutoscalerBehavior` verbatim (§4); its *shape* is fixed here, its *algorithm* is the stabilization proposal's.
- **`analyzers`** *(per-model, tenant-tunable)* — the saturation/queueing analysis plugins and their thresholds (§4). Per-**role** (prefill/decode) tuning lives *inside* an analyzer's `parameters`, not as top-level fields (§5).

`limiters`/`quota` are **not** in that list — they are admin-only and cluster-scoped (§2).

## 2. Scope tiers — one schema, up to three placements

The surface is **one schema**, resolved at up to **three placements of it** — not three schemas, not three kinds — by a deterministic lookup in fixed order. In phase 1 each placement is a ConfigMap; in phase 2 each is a `ScalingPolicy` object.

1. **Cluster-default** — in the system namespace, no `inferencePoolRef`. **Admin-owned.**
2. **Namespace-default** — in the workload's namespace, no `inferencePoolRef`. **Tenant-owned.**
3. **Per-pool** — in the workload's namespace, `inferencePoolRef` set. **Tenant-owned.** A workload-specific policy is *just this placement* — it needs only the fields it overrides.

**How the controller finds a tier (phase 1).** A CRD has identity from its kind, namespace, and `spec.inferencePoolRef`; a ConfigMap does not, so phase 1 classifies ConfigMaps by a fixed label set the controller watches (`scaling.llm-d.ai/config: "true"`):

| tier | namespace | labels |
|---|---|---|
| cluster-default | system (`workload-variant-autoscaler-system`) | `scaling.llm-d.ai/tier: cluster` |
| namespace-default | workload's | `scaling.llm-d.ai/tier: namespace` |
| per-pool | workload's | `scaling.llm-d.ai/tier: pool`, `scaling.llm-d.ai/pool: <inferencePool>` |

The controller resolves a workload's policy from the labelled ConfigMaps in (system-ns, workload-ns); the `inferencePoolRef` inside the document must agree with the `pool` label (admission-checked). In phase 2 this labelling goes away — the CR's kind + namespace + `spec.inferencePoolRef` are the identity.

**Resolution is inheritance, not replacement.** Fields merge **cluster → namespace → per-pool**, higher tier winning **per field** — a per-pool object that sets only `priority` inherits `scaleToZero`, `behavior`, and `analyzers` from the tiers above it. `analyzers` merge **by `name`** (§4): a lower tier can add or replace an analyzer entry without redeclaring the others. Note the granularity: because a plugin's `parameters` is **opaque** to the envelope (§4), it cannot be structurally deep-merged — a same-`name` entry **replaces the whole `parameters` block**, it does not patch one key. So a tenant writes the minimum set of *entries*, but retuning one threshold means restating that analyzer's `parameters`. This is the "narrower than intended" point from review: real per-field/per-entry inheritance across tiers, with parameters as the replace-granular leaf.

**One kind, two purposes — deliberately.** The same schema serves two owners because it is placed differently and gated by different RBAC: at the **cluster-default** tier it is the admin's **enforcement** carrier (limiters/quota); at the **namespace/per-pool** tiers it is the tenant's **tuning** surface (priority, scaleToZero, analyzers). This dual purpose is intentional — one schema and one merge, partitioned by placement and RBAC, rather than two objects to keep in sync.

**Which fields live at which tier:**

| field family | tiers allowed | owner | rationale |
|---|---|---|---|
| `analyzers`, `priority`, `scaleToZero` | any tier | tenant (ns/pool), admin sets cluster defaults | per-model tuning; safe to delegate |
| **`limiters` / `quota`** | **cluster-default only** | **admin** | instance-wide **enforcement**, not a tenant knob |

**`limiters`/`quota` are cluster-default-only, and this is a security boundary, not a style choice.** They are excluded from the merge entirely — a lower tier can neither override nor remove them. That is why **`inferencePoolRef` and `limiters` never co-occur**: `limiters` exist only on the cluster-default, which by definition has no `inferencePoolRef`. If a tenant could set `limiters` on a policy they own, they could drop the `quota` limiter (or set `limiters: []`) and **escape their own cap**.

The isolation **guarantee** is RBAC plus the controller; admission is only UX:

1. **RBAC (the guarantee)** — tenants have no permission to write the cluster-default (it lives in the system namespace). Identical in both phases; this is what actually prevents cap-escape.
2. **Controller (defense in depth)** — WVA only honors `limiters`/`quota` from the cluster-default tier; a `limiters` block on a tenant-tier object is *ignored*, so even a mis-scoped config cannot weaken enforcement.
3. **Admission (feedback, not a control)** — a **`ValidatingAdmissionPolicy` (CEL)** rejects a namespace/per-pool config carrying `limiters` so the tenant gets an immediate error instead of a silent ignore. In phase 1 this is a string-level check on the ConfigMap document (a regex, not a security boundary); phase 2's CRD `x-kubernetes-validations` makes it field-precise (§6). It is convenience over (1)+(2), never a substitute for them.

**`analyzers` and `limiters` type selection vs. the controller's global mode.** The analyzer *type* a policy names (`saturation`, `queueing`) selects a plugin; whether the engine runs the V1 or V2 analysis pipeline, and whether the GPU limiter is active at all, are **controller-global modes** (today: the `enableLimiter` / analyzer-version switches), not per-policy fields. A policy *configures* the analyzers/limiters the controller is running; it does not turn the pipeline on or off. Keeping that boundary explicit answers "is the analyzer type global?" — the *pipeline* is; the *plugin selection and parameters* are policy.

**Two objects matching the same scope** (two per-pool policies for one pool, or two namespace-defaults in one namespace) are a configuration error. This is a *set* invariant — admission (VAP or CRD CEL) validates one object in isolation and cannot see siblings — so it is **detected by the controller**, which refuses to resolve an ambiguous scope and raises a `Conflict` (a duplicate on a status-bearing object; a controller log/event in the status-less phase-1 ConfigMap case) rather than picking one non-deterministically.

## 3. Relationship to the `ScaledObject` (review question 1)

WVA and the scaler own **different things, on different objects**:

- **WVA** computes the scaling **decision** and publishes it as the `wva_desired_replicas` **metric**. It configures *its own* decision via this schema. It does **not** create, own, or write the `ScaledObject`, and never sets `spec.replicas`.
- The **KEDA `ScaledObject`** (the scaler WVA integrates with) is a **standalone autoscaler** owned by the tenant. It actuates `spec.replicas` from whatever metrics it is configured with — WVA simply supplies one of them (`wva_desired_replicas`). It can scale without WVA; WVA feeds it a signal.

So there is **no configuration cross-reference in either direction**: the config points at the `InferencePool`, not the `ScaledObject`; the `ScaledObject` references the metric, not any WVA config object. Configuring WVA never means editing a tenant's `ScaledObject`.

```
ScalingConfig ─configures→ WVA ─emits→ wva_desired_replicas ─read by→ ScaledObject ─sets→ spec.replicas
```

Consequences:

- **`maxReplicas` stays** as the tenant's hard per-workload ceiling on the `ScaledObject`. WVA's desired count is clamped by it exactly as today; the config sets the *policy* WVA decides within, not the safety cap.
- **Scale-to-zero** — `scaleToZero.enabled` lets WVA emit a desired of `0`; the `ScaledObject` (with KEDA's idle-replica support) actuates it.
- **`behavior` means WVA owns stabilization — the `ScaledObject` must not double it.** `spec.behavior` (§4) makes WVA apply the HPA-style damping (windows, rate policies) to its *own* recommendation before emitting it. But the `ScaledObject`/HPA can *also* stabilize the metric it reads, and two stabilizers in series **compound lag** (a spike is damped by WVA, then damped again downstream). The contract when `behavior` is set: **WVA owns stabilization; the `ScaledObject`/HPA `behavior` is configured near-passthrough** (`stabilizationWindowSeconds: 0`). Where the two are reconciled (operator convention vs. WVA generating the `ScaledObject` behavior) is the stabilization proposal's, not this schema's — this proposal only fixes that `behavior` is a per-model field and that it is the *single* stabilizer of record.
- **Managed-set identification is unchanged.** WVA discovers the workloads it manages from the `llm-d.ai/managed` label on the `ScaledObject` (and reads `llm-d.ai/model-id`, `inference.optimization/acceleratorName`) — the current mechanism. This proposal does not change it (§7).

## 4. The generic schema shape (review question 2)

The schema is a **thin typed envelope around pluggable, schemaless plugin lists**, so the envelope is small and fixed and new plugins never change it:

```yaml
# (a) cross-cutting fields — typed, well-known schemas, validated
inferencePoolRef: { name: granite-premium-pool }   # match key (per-pool tier only)
priority: 2.0
scaleToZero: { enabled: false }
behavior:                      # k8s.io/api/autoscaling/v2 HorizontalPodAutoscalerBehavior, verbatim
  scaleDown: { stabilizationWindowSeconds: 300, policies: [ { type: Percent, value: 100, periodSeconds: 60 } ] }

# (b) plugin lists — {type, name, parameters}; parameters is plugin-owned
analyzers:                     # per-model, tenant-tunable, any tier
  - type: saturation
    name: sat                  # identity for cross-tier merge; defaults to `type`
    parameters:                # opaque to the envelope; each plugin validates its own
      scaleUpThreshold: 0.95
limiters:                      # cluster-default only (§2)
  - type: gpu-inventory
  - type: quota                # parameters owned by the quota work (#1162)
```

- **(a) cross-cutting fields** (`priority`, `scaleToZero`, `behavior`, `inferencePoolRef`) are the small stable set. Each has a **fixed, well-known schema** — `behavior` in particular is `autoscaling/v2` `HorizontalPodAutoscalerBehavior` reused verbatim (a k8s building block, not a WVA invention). In **phase 2** they are OpenAPI-typed and `kubectl explain`-able; in **phase 1** the controller validates them on load and a `ValidatingAdmissionPolicy` guards the coarse shape.
- **(b) plugin lists** are name-keyed `{type, name, parameters}` — the **EPP `EndpointPickerConfig` shape** reused so the plugin-list convention is one WVA already ships as an InferencePool consumer, not a bespoke one. **`parameters` is opaque to the envelope** (phase 2: `x-kubernetes-preserve-unknown-fields`) — each plugin validates its own. `name` keys the cross-tier merge (replace-granular, §2), **defaults to `type`** for a single instance, and must be unique within a tier.

**This is what keeps the surface generic — and why one config object suffices rather than a new CRD per feature.** Adding an analyzer or limiter ships *its own* `parameters` keys; the envelope does not change. A future `optimizer` block slots in the same way with no redesign. This directly answers the "won't we keep needing new CRDs / should the kind be more general" concern: the envelope *is* the generality — the plugin list absorbs new config without a schema change, so a single `ScalingPolicy` (or its phase-1 ConfigMap) does not need to become a broader kind to grow.

**Schema vs. engine — the boundary this proposal draws.** This document fixes the **schema and resolution** only: the fields, the tiers, the merge, the plugin envelope, and the `status` *shape*. It does **not** specify engine behavior — how an analyzer resolves a threshold, how per-role parameters are applied, how the optimizer consumes `priority`. Those are engine concerns, tracked separately, and are deliberately left open so the schema can be agreed without pinning the implementation.

## 5. Per-role (prefill/decode) configuration

Per-role tuning is a real need for disaggregated (P/D) models, and it has a clear home: **inside an analyzer's `parameters`** (e.g. a `roleOverrides` block), **not** as top-level fields. The reason is ownership of the concern — `priority` and `scaleToZero` are **per-model** decisions (the whole model scales to zero, or has a priority), while role-specific thresholds are **per-analyzer** knobs. So the envelope carries no `prefill`/`decode` fields; a P/D model configures both roles through the analyzer plugin that understands them:

```yaml
analyzers:
  - type: saturation
    parameters:
      roleOverrides:
        prefill: { scaleUpThreshold: 0.90 }
        decode:  { scaleUpThreshold: 0.95 }
```

*Applying* per-role thresholds is an **engine** change (resolving and enforcing role-scoped parameters), out of scope here and tracked separately — this proposal only fixes that per-role config is a `parameters` concern, so it needs no envelope change.

## 6. Surfacing the resolved configuration (status) — and the CRD's phase-2 value

An operator must be able to see *what WVA actually decided* for a pool — the merged policy and the workloads it governs — without reverse-engineering the merge. The **shape** of that surface:

```yaml
status:
  effectivePolicy: { … }          # merged result (cluster → ns → per-pool)
  sources: [ … ]                  # which tier each field came from, in precedence order
  managedVariants:                # workloads WVA manages in this pool (managed set, §7)
    - workloadRef: { kind: Deployment, name: granite-premium-prefill }
      role: prefill               # when known
      cost: 40.0                  # per-replica $/hr — the stable cost input, surfaced here
  conditions: [ { type: PolicyMatched, … } ]   # False when no managed workload matched
```

- **`effectivePolicy` + `sources`** make the merge auditable in one place.
- **`managedVariants`** is the policy→workload mapping (distinct from pool membership — an unmanaged pool member does not appear); `cost` is surfaced so the basis of each scaling decision is visible. *Which* workloads and *how* they are identified, and where `cost` is sourced, are the VA-deprecation proposal's (§7) — this fixes only the shape.

**Be explicit: phase 1 is config-only — this visibility does not exist yet.** A ConfigMap has no `.status`, so in phase 1 there is **no `kubectl get` view of the merged policy**; the resolved configuration is surfaced (if at all) by a `wva-config explain <pool>` command or a WVA-maintained status-only object, otherwise deferred. Do not expect the effective-policy surface in phase 1. **This is the concrete reason the CRD earns phase 2:** promoting to the CRD gives a **native, per-object `.status`**, plus **field-level typed validation** (`x-kubernetes-validations` walking the actual fields, vs. phase-1 string checks on the ConfigMap blob) and **`kubectl explain`**. Security is *not* on that list — RBAC + controller enforcement (§2) already secure phase 1 — so the CRD is a validation/UX/observability upgrade, taken when its cost is justified, not a prerequisite.

**Open question — where the effective config is shown for a pool with no per-pool object.** Per-pool objects are optional, so a pool governed by the namespace/cluster default alone has no per-object status in phase 2 either. Candidate homes: the `InferencePool` status, a WVA status-only object per managed pool, or the `wva-config explain` view. Unresolved; the same answer serves both phases.

## 7. Dependencies and out of scope

**Managed-set identification and per-variant cost are not decided here.** How WVA identifies the workloads it manages, and where per-replica `cost` comes from, are owned by the **VA-deprecation proposal**. For context: managed workloads are marked with the `llm-d.ai/managed` label on their `ScaledObject`, which WVA reads; per-variant `cost` comes from the `VariantAutoscaling` CR until that proposal relocates it. This proposal fixes only the **shape** of `managedVariants` and is independent of the mechanism.

Deferred (a later revision or companion proposal):

- **Migration/tooling** — a `wva-config` CLI, ConfigMap→CRD promotion, deprecation timeline.
- **The full `status` catalog** beyond `effectivePolicy`/`sources`/`managedVariants`/`PolicyMatched` — extra conditions, timestamps, observed generations.
- **Per-role application** — the engine change to resolve and enforce analyzer `roleOverrides` (§5).
- **The `quota` `parameters` schema and enforcement** — owned by the quota work (#1162 / issue #1002).
- **Alternatives and autoscaler comparison matrices.**

Once the schema (§4), the tier resolution (§2), and the `ScaledObject` boundary (§3) are agreed, these layer on top without changing any of them — in either the ConfigMap or the CRD phase.
