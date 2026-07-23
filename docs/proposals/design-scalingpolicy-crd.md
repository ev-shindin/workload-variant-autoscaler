# Proposal: WVA scaling-configuration schema — ConfigMap first, `ScalingPolicy` CRD later

## Scope

This fixes the bare-minimum core of WVA's scaling configuration: the **schema** (fields + a pluggable plugin envelope) and its **resolution**. It answers the two questions that gate the design:

1. **How does the config relate to the workload's autoscaler?** (§2)
2. **What is the generic shape of the config?** (§3)

It lands in **two phases** (see *Delivery*): **Phase 1** adds the schema to today's ConfigMap; **Phase 2** promotes it to a `ScalingPolicy` CRD. Migration tooling, per-role detail, the full status catalog, and alternatives are **out of scope** (§6).

**What approving this ratifies:** the off-the-`ScaledObject` boundary (§2) and the schema shape (§3), and a green light for the Phase-1 ConfigMap extension. The Phase-2 CRD is **directional** — its shape is fixed here, but the delivery mechanics (validation, admission, the status surface) land in follow-ups and are called out inline where they're still open.

> **"Schema"** = the fixed set of typed fields (`priority`, `scaleToZero`, `inferencePoolRef`) plus a pluggable `{type, name, parameters}` list for `analyzers`/`limiters`, where `parameters` is plugin-owned. Adding an analyzer or limiter ships its own `parameters` and never changes this schema.

## What exists today, and what each phase adds

**Today.** WVA is configured by **one cluster-wide, admin-owned ConfigMap** (`saturation-scaling-config`): a `default` entry plus per-(model, namespace) override entries carrying `priority` and thresholds. Scale-to-zero and analyzer selection are controller config. A workload is *scaled* by WVA publishing a `wva_desired_replicas` metric that the workload's KEDA `ScaledObject` reads — and **the config is optional**: a workload with only the managed labels scales under WVA's defaults, and its `ScaledObject` scales it even without WVA.

**Phase 1 (ConfigMap) adds the *schema* — plus one behavior rule.** The new fields and the `{type, name, parameters}` plugin envelope become recognized in the *same* admin-owned ConfigMap. It stays one admin-managed object: `priority` stays per-model, existing entries keep working, adoption is additive and per-entry. Three things about Phase-1 semantics are worth calling out plainly:

- **Precedence (a genuine behavior change, not just schema).** For `scaleToZero` and analyzer selection, a value set in the ConfigMap wins; the controller-config setting is the fallback used when the field is absent.
- **`limiters` are honored in Phase 1.** Phase 1 is all-admin, so a `limiters` block in the ConfigMap takes effect. The `limiters`-only-on-cluster-default *boundary* (§3) begins in Phase 2 — it's a placement rule, not a Phase-1 restriction. Only an entry naming a limiter *type* that isn't implemented yet is ignored.
- **No validation.** Phase 1 ships no admission/CEL, so an unrecognized or Phase-2-only field (e.g. `inferencePoolRef`) is silently ignored, not rejected — typed validation is exactly what Phase 2 adds.

Adopting the envelope keeps the legacy flat form working, so the loader parses **both** the flat and the `{type, parameters}` shapes for as long as Phase 1 lives (see *Before / after*). (Per-variant `cost` is unchanged — VA-sourced, §5, not a config field.)

**Phase 2 (CRD)** adds what a ConfigMap does poorly or not at all — typed validation, native `.status`, a single discoverable kind, and a clean per-object ownership boundary. The CRD's reason to exist is spelled out in §Delivery.

**Before / after — Phase 1, same surface.** Today:

```yaml
# ConfigMap saturation-scaling-config (admin-owned), data:
default: |
  priority: 1.0
  scaleUpThreshold: 0.85
premium-override: |
  model_id: granite-premium
  namespace: production
  priority: 2.0
  scaleUpThreshold: 0.95
```

After Phase 1 — the **same ConfigMap**. Adoption is **per entry and optional**: `default` is left untouched (the flat top-level `scaleUpThreshold` binds to WVA's default saturation analyzer and still parses), and `premium-override` opts into the new schema — adding the genuinely-new fields (`scaleToZero`, the `analyzers` plugin envelope) and re-expressing the same `scaleUpThreshold` explicitly under it:

```yaml
default: |                     # UNCHANGED — flat legacy form still parses
  priority: 1.0
  scaleUpThreshold: 0.85
premium-override: |            # opted into the new schema (same values)
  model_id: granite-premium
  namespace: production
  priority: 2.0
  scaleToZero: { enabled: false }
  analyzers: [ { type: saturation, parameters: { scaleUpThreshold: 0.95 } } ]
```

No new object, no install step, no forced rewrite — that is the whole of Phase 1. **Its worth:** it lands and de-risks the pluggable analyzer/limiter *schema* and moves `scaleToZero`/analyzer selection onto config, usable now, before committing to the CRD's validation/admission machinery.

## Delivery — ConfigMap first (Phase 1), CRD later (Phase 2)

- **Phase 1 — extend the existing ConfigMap** with the schema above. Admin-managed, additive, no CRD/migration/install. **What it does *not* deliver:** the tenant/admin ownership boundary, per-pool granularity, typed validation, or status — see why below.
- **Phase 2 — promote to the `ScalingPolicy` CRD** (§1–§5), which adds those.

**Why the CRD (not just more ConfigMaps).** The concrete driver is the ownership boundary. Kubernetes RBAC is **per-object, not per-field**: in one shared ConfigMap whoever can write the object writes *every* field — so a tenant allowed to tune `priority`/thresholds could also set `limiters: []` and **escape their own cap**. The boundary is closed by the `limiters`-only-on-cluster-default **merge rule** (§4) plus an **admin-owned** object holding them. That boundary does *not*, by itself, require a CRD — per-namespace ConfigMaps (tenant) + a cluster ConfigMap (admin) separate objects too. What a CRD **uniquely** adds is a **typed, discoverable, single-kind** surface: OpenAPI validation + `kubectl explain`, and a native `.status`. Ownership and per-pool matching are *cleaner* with the CRD but not impossible without it. The one unqualified win is **typed validation** — a ConfigMap's only check is a controller parse. Native `.status` is a *partial* win: it gives a resolved-policy view for pools that have an explicit per-pool object, but the common default-only case has no such object and its status surface is still open (§4 caveat, §5). So the honest CRD case rests first on typed validation, with status as a follow-on for pools that opt into a per-pool object. So Phase 1 stays admin-owned (as today's single ConfigMap already is); **tenant self-service and the ownership boundary arrive in Phase 2.** Its field *schema* (§3) matches Phase 1's — the one part Phase 1 does **not** de-risk is the **match identity**: Phase 1 keys entries by `model_id`+`namespace`, the CRD matches by `inferencePoolRef` → pool (§1).

## 1. The object (Phase 2)

One namespaced CRD, `ScalingPolicy` (`scaling.llm-d.ai/v1alpha1`). It is matched to a workload through that workload's **`InferencePool`** (Gateway API Inference Extension; the workload→pool link is the pool's pod selector), **replacing Phase 1's `model_id`+`namespace` keying** — not through the workload or its autoscaler. The referenced pool is assumed co-namespaced with the `ScalingPolicy` (`inferencePoolRef` carries no namespace).

```yaml
apiVersion: scaling.llm-d.ai/v1alpha1
kind: ScalingPolicy
metadata:
  name: granite-premium            # object name is free-form; the match is via inferencePoolRef below
  namespace: production
spec:
  inferencePoolRef: { name: granite-premium-pool }
  priority: 2.0
  scaleToZero: { enabled: false }
  analyzers:
    - type: saturation
      parameters: { scaleUpThreshold: 0.95 }
```

The policy **points at the pool**; the pool's workloads carry no reference back. *(The kind is **`ScalingPolicy`** — the name this proposal commits to.)*

## 2. Relationship to the workload's autoscaler (question 1)

WVA drives scaling through a **KEDA `ScaledObject`**, and they own different things: **WVA owns the scaling decision; the `ScaledObject` owns actuation.** The config (ConfigMap or `ScalingPolicy`) configures the decision; the `ScaledObject` actuates it by reading WVA's `wva_desired_replicas` external metric. The decisive point: **the `ScaledObject` can scale its workload independently of WVA** — it is a general autoscaler that happens to read a metric WVA publishes — so WVA's config must live *off* it.

| | Owns | Reads | Writes |
|---|---|---|---|
| **KEDA `ScaledObject`** (tenant-owned) | replica **actuation** | the `wva_desired_replicas` metric | the workload's `spec.replicas` |
| **WVA controller** | the scaling **decision** | the config + cluster/pool state | the `wva_desired_replicas` metric (+ `ScalingPolicy.status` in Phase 2) |
| **config** (ConfigMap → `ScalingPolicy`) | the scaling **policy** (priority, thresholds, scale-to-zero, `limiters` incl. `quota`) | — | — |

Consequences: WVA config never lands on the tenant's autoscaler object; WVA never writes `spec.replicas` or edits the `ScaledObject` (it only publishes the metric); the `ScaledObject`'s `maxReplicas` stays as the tenant's final hard ceiling; and `scaleToZero.enabled` lets WVA emit a desired of 0 that KEDA actuates. *(How a workload is identified as WVA-managed is settled by the VA-deprecation proposal, §5, and does not affect this shape.)*

## 3. The generic schema shape (question 2)

A **thin typed envelope around pluggable plugin lists** (per the *Schema* definition above). In **Phase 1** every field lives in the one admin ConfigMap (admin-set); the **tier** and **owner** columns below are the **Phase-2** world:

| Field | Phase-2 tier | Owner | Typed? |
|---|---|---|---|
| `inferencePoolRef` | per-pool only | tenant | typed |
| `priority` | any tier | tenant — **write-blocked until admin cap ships (§5)** | typed |
| `scaleToZero` | any tier | tenant | typed |
| `analyzers[]` `{type,name,parameters}` | any tier | tenant | envelope typed; `parameters` plugin-owned |
| `limiters[]` (incl. `quota`) | **cluster-default only** | **admin** | envelope typed; `parameters` plugin-owned |

Two valid objects, because `inferencePoolRef` and `limiters` never co-occur — a per-pool object (tenant) and the cluster-default (admin):

```yaml
# (a) per-pool object (tenant tier) — tunes an analyzer, no limiters
spec:
  inferencePoolRef: { name: granite-premium-pool }
  priority: 2.0
  scaleToZero: { enabled: false }
  analyzers:
    - type: saturation
      name: sat            # identity for cross-tier merge; defaults to `type`
      parameters: { scaleUpThreshold: 0.95 }          # x-kubernetes-preserve-unknown-fields
---
# (b) cluster-default object (admin tier) — carries limiters, no inferencePoolRef
spec:
  limiters:
    - type: gpu-inventory
    - type: quota          # a limiter type; parameters owned by the quota work
```

- **`parameters` is `x-kubernetes-preserve-unknown-fields`** — each plugin validates its own at load. `name` keys the cross-tier **parameter** merge and defaults to `type`; required only for two instances of one `type`.
- **`limiters` are cluster-default-only** (instance-wide enforcement, not tenant knobs — the ownership boundary from §Delivery). The boundary is closed by the **merge rule**: the engine reads `limiters` only from the cluster-default object, so a tenant copy is inert. A CEL rule *rejecting* them outright is Phase-2 hardening on top (§6), not what closes the hole.
- **`analyzers`: per-tier *parameter tuning* is fine; per-tier *type selection* is engine-future.** Tuning an active analyzer's `parameters` at a lower tier — what every example here does (e.g. a per-pool `scaleUpThreshold`) — merges normally, keyed by `name`. But **changing *which* analyzer types run per tier** (enabling/disabling types) is not yet honored: the engine applies one type-selection today, so Phase 2 **surfaces a per-tier type-selection change in `.status`** rather than silently accept a field the engine drops. (Rejecting it at admission is possible later hardening, not the Phase-2 behavior.)

## 4. Tiers (Phase 2 — why the CRD is more than one object)

Phase 1's single admin ConfigMap already holds a `default` entry **plus per-(model, namespace) in-data overrides** — all admin-owned. Phase 2 turns those into the *same* CRD resolved at up to **three object placements**, and adds the per-object **tenant ownership** and the **per-pool** tier a ConfigMap can't (per §Delivery):

1. **Cluster-default** — a namespaced object in the system namespace, no `inferencePoolRef` (admin). ("Cluster-default" names its *role*, not a cluster-scoped resource.)
2. **Namespace-default** — the workload's namespace, no `inferencePoolRef` (tenant).
3. **Per-pool** — the workload's namespace, `inferencePoolRef` set (tenant).

Fields merge cluster → namespace → per-pool (higher wins); a lower tier tunes an analyzer's `parameters` by `name`. `limiters`/`quota` are **excluded** — cluster-default only. Per-tier **analyzer type-selection** is engine-future — ignored by the engine and surfaced in `.status` (§3). Two objects at the same placement (two cluster-defaults, two namespace-defaults in one namespace, or two per-pool objects on the same `inferencePoolRef`) are a config error, caught by the **WVA controller at reconcile** — a cross-object rule, so not expressible in OpenAPI/CEL alone; a validating webhook is optional later hardening.

### Status (Phase 2)

The per-pool object's `.status` publishes `effectivePolicy` (the merged result) + `sources` (contributing tiers) and `managedVariants` (the workloads it governs, each with `workloadRef`, `role`, `cost`; empty ⇒ `PolicyMatched: False`). **Caveat:** this only shows on pools that *have* a per-pool object; a pool governed solely by a higher-tier default has **no per-pool object and thus no resolved-policy view** — the common case. Where to publish it there is open (§5), so "`kubectl get scalingpolicy` shows what WVA decided" holds only for pools with an explicit object.

## 5. Dependencies and open questions

- **Depends on the VA-deprecation proposal.** It owns how WVA identifies managed workloads and where per-variant `cost` comes from; this proposal fixes only the `managedVariants` **shape** (`workloadRef`, `role`, `cost`). Because that shape leans on an undecided proposal, treat the status surface as **shape-settled, source-pending**.
- **Cross-tenant `priority` bound (open).** `priority` is tenant-owned yet compared on **one global scale** across namespaces when WVA allocates scarce GPUs. That is the mirror of the `limiters` hole: nothing yet stops a tenant setting `priority: 1e9` to starve other tenants. Phase 2 needs an admin-set cap or normalization on tenant priority. The schema and tiers may land first, but **tenant-writable `priority` is blocked until that cap/normalization ships** — until then `priority` stays admin-set.
- **Effective policy for default-only pools (open).** As above (§4 status caveat): where the resolved policy is shown for a pool with no per-pool object. Candidates: the `InferencePool` status, a WVA-maintained status object, or a `wva-config explain <pool>` view.

## 6. Deliberately out of scope

- The Phase-1→Phase-2 **promotion/migration** mechanics (the `wva-config` tool) and the ConfigMap **removal timeline** — the two-phase delivery is above; its tooling and end-state are deferred.
- **Extra CEL/admission *rules*** — cross-field constraints beyond the CRD's inherent OpenAPI typing (which *is* Phase 2). These are defense-in-depth on top of the merge-rule boundary (§Delivery), not what it depends on.
- The full `.status` schema beyond the fields above (extra conditions, timestamps, observed generations).
- Per-role (prefill/decode) configuration — an analyzer-`parameters` concern, not a schema-shape one.
- Alternatives considered and autoscaler comparison matrices.
- The `quota` limiter's `parameters` schema and enforcement — owned by the quota work.

Once the boundary (§2) and shape (§3) are agreed, these layer on top without changing either.
