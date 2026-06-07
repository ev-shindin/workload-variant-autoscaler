# Proposal: ScalingPolicy CRD — minimal core

## Scope

This is the **bare-minimum** core of the WVA configuration CRD, split out from the broader analysis in **#1194** (kept open for reference). Per review, it answers the two questions that gate the design — and settles only the minimum they force (the resolution tiers and the status surface):

1. **What is the relationship between the Config CRD and the HPA object?** (§2)
2. **What is the generic shape (schema) of the CRD?** (§3)

Everything else from #1194 — migration tooling, deprecation phases, the full status-condition catalog, per-role detail, alternatives, comparison matrices — is **deliberately deferred** (§6); the dependency on the VA-deprecation proposal (discovery, cost source) and one remaining open question are in §5. The goal here is to agree the CRD's *boundary* and *shape* first; the rest layers on top without changing either.

## 1. The object

One namespaced CRD, `ScalingPolicy` (`scaling.llm-d.ai/v1alpha1`), carries WVA's scaling configuration. It is matched to a workload through that workload's **`InferencePool`** (defined by the Gateway API Inference Extension; the workload→pool link is the pool's pod selector) — not through the workload, and not through its HPA:

```yaml
apiVersion: scaling.llm-d.ai/v1alpha1
kind: ScalingPolicy
metadata:
  name: granite-premium-priority
  namespace: production
spec:
  inferencePoolRef:
    name: granite-premium-pool      # the pool this policy configures
  priority: 2.0
  scaleToZero: { enabled: false }
  analyzers:
    - type: saturation
      parameters: { scaleUpThreshold: 0.95 }
```

The policy **points at the pool**; the pool's workloads carry no reference back to the policy. Why that direction matters is §2.

## 2. Relationship to the HPA / ScaledObject (review question 1)

**They own different things:** WVA owns the scaling decision, the HPA owns actuation. `ScalingPolicy` configures the decision; the HPA actuates it by reading WVA's `wva_desired_replicas` metric. Neither the policy nor the HPA references the other's *config*. WVA reads the HPA/`ScaledObject` to identify the workloads it manages, but the *mechanism* (the #1130 label) is the **VA-deprecation proposal's** — and this CRD's shape does not depend on it.**

| | Owns | Reads | Writes |
|---|---|---|---|
| **HPA / KEDA `ScaledObject`** (tenant-owned) | replica **actuation** | the `wva_desired_replicas` metric | the workload's `spec.replicas` |
| **WVA controller** | the scaling **decision** | `ScalingPolicy`, cluster/pool state, and the HPA/`ScaledObject` that marks its managed workloads (per the VA-deprecation proposal) | the `wva_desired_replicas` metric (+ `ScalingPolicy.status`) |
| **`ScalingPolicy` CRD** | the scaling **policy** (priority, thresholds, scale-to-zero, quota) | — | — |

The data flow is one-directional:

```
ScalingPolicy ──configures──▶ WVA ──emits──▶ wva_desired_replicas ──read by──▶ HPA / ScaledObject ──sets──▶ spec.replicas
```

The consequences answer the question directly:

- **No config cross-reference.** The HPA / `ScaledObject` carries no reference to a WVA *config* object, and `ScalingPolicy` points at the `InferencePool`, **not** the HPA — so configuring WVA never means editing a tenant's HPA. The HPA's tie to WVA is that it reads the `wva_desired_replicas` metric to actuate. (An `HPA → ScalingPolicy` config reference would put a WVA-specific field on every workload — that's what this avoids.)
- **WVA never *writes* tenant-owned objects.** It does not set `spec.replicas` or modify the HPA / `ScaledObject`; it publishes the `wva_desired_replicas` metric, which the HPA consumes. WVA *does* **read** the HPA/`ScaledObject` to identify its managed set — the mechanism (the `llm-d.ai/managed` label on those objects, #1130) belongs to the VA-deprecation proposal, not this one.
- **HPA `maxReplicas` stays, as a complementary hard ceiling.** `ScalingPolicy` does not replace it: WVA's desired count is clamped by the HPA exactly as today. `ScalingPolicy` sets the *policy* WVA decides within; `maxReplicas` is the tenant's final per-workload safety cap.
- **Scale-to-zero is a policy toggle, actuated by the HPA layer.** `spec.scaleToZero.enabled` lets WVA emit a desired of 0; KEDA/HPA actuate it. WVA decides 0; it does not force it.

**Net:** a workload is *configured* via its pool (§1's "not through the HPA") and *actuated* via the metric; the CRD sits *upstream* of that metric, so it slots into the existing HPA/KEDA flow without modifying it. How a workload is *identified as WVA-managed* is the VA-deprecation proposal's, not here.

## 3. The generic schema shape (review question 2)

The CRD is a **thin typed envelope around pluggable, schemaless plugin lists** — so the schema itself is small and fixed, and new analyzers/limiters never change it. Two validation tiers, deliberately:

```yaml
spec:
  # (a) typed, cross-cutting fields — OpenAPI/CEL-validated, `kubectl explain`-able
  inferencePoolRef: { name: granite-premium-pool }   # per-pool only; mutually exclusive with limiters (§4)
  priority: 2.0
  scaleToZero: { enabled: false }

  # (b) pluggable lists — {type, name, parameters}; `parameters` is plugin-owned
  analyzers:                          # one or more; TENANT-tunable, any tier
    - type: saturation
      name: sat                       # identity, used for cross-tier merge
      parameters:                     # x-kubernetes-preserve-unknown-fields
        scaleUpThreshold: 0.95
  limiters:                           # zero or more; chain; CLUSTER-DEFAULT ONLY (admin)
    - type: gpu-inventory
    - type: quota                     # parameters schema owned by #1162
```

*Field catalog, not a single valid object: a real `ScalingPolicy` is one tier, and `inferencePoolRef` and `limiters`/`quota` never co-occur — limiters live only on the cluster-default, which has no `inferencePoolRef` (§4).*

- **(a) Stable typed fields** — the tunable cross-cutting fields (`priority`, `scaleToZero`) and the match key (`inferencePoolRef`) are **typed**: OpenAPI/CEL-validated, discoverable via `kubectl explain`.
- **(b) Plugin lists** (`analyzers`, `limiters`) are name-keyed `{type, name, parameters}` where **`parameters` is `x-kubernetes-preserve-unknown-fields`** — each plugin validates its own at load. This reuses the **EPP `EndpointPickerConfig` shape** (not its config — both sides merely read the `InferencePool`). `name` keys the cross-tier merge (§4) and **defaults to `type`** for a single instance; it's required only for two instances of one `type`, and must be unique within a tier (admission-enforced).
- **Tenant-tunable vs admin-only — an isolation boundary, not a style choice.** `analyzers`, `priority`, and `scaleToZero` are tenant-tunable at any tier. **`limiters` (and `quota`) are cluster-default-only**: they are *instance-wide enforcement*, not tenant knobs. A **CEL rule rejects `limiters`/`quota` on any namespace or per-pool `ScalingPolicy`**, and tenants have no RBAC to write the cluster-default object (it lives in the system namespace). This is load-bearing for security: if a tenant could set `limiters` on a policy they own, they could drop the `quota` limiter — or set `limiters: []` — and **escape their own cap**. So `limiters`/`quota` are never authored, merged, or overridable below the cluster-default tier (§4).

The one property that makes the shape "generic": **adding an analyzer or limiter ships its own `parameters` keys and the CRD schema does not change.** The optimizer is a single fixed stage with nothing to configure today; the schema leaves room for a `spec.optimizer` block later with no redesign.

(The `quota` limiter's `parameters` schema and its cluster-scope enforcement are owned by **#1162** / issue #1002 — quota lives on this surface, but its shape is settled there, not here.)

## 4. Why more than one tier (the quota constraint)

The surface is still **one CRD**. It is resolved at up to **three placements** of that same object — not three schemas and not three kinds — by a deterministic lookup in fixed order:

1. **Cluster-default** — `ScalingPolicy` in the system namespace, no `inferencePoolRef`.
2. **Namespace-default** — `ScalingPolicy` in the workload's namespace, no `inferencePoolRef`.
3. **Per-pool** — `ScalingPolicy` in the workload's namespace, `inferencePoolRef` set.

Fields merge cluster → namespace → per-pool (higher tier wins; `analyzers` merge by `name`). **`limiters` and `quota` are excluded from the merge entirely — they exist *only* on the cluster-default tier** (CEL-rejected on the others, §3), so a lower tier can neither override them nor remove them. A tenant cannot weaken or delete their own cap by editing a policy they own; the only objects they can write (namespace / per-pool) cannot carry a limiter at all.

Two `ScalingPolicy` objects matching the **same** scope (two per-pool policies for one pool, or two namespace-defaults in one namespace) are a configuration error, **rejected at admission** — so resolution is always deterministic, never order- or name-dependent.

**Two tiers are forced by ownership, not preference.** Quota is **cluster-admin-owned and cluster-scoped** (a tenant must not raise their own cap, and a cluster aggregate needs a cluster-wide view — per-namespace instances would each see full node inventory and collectively overcommit); thresholds/`priority`/`scaleToZero` are **tenant-owned and namespace-scoped**. Different owners, different RBAC — they can't share one object without letting tenants edit quota or admins own every tenant's thresholds. So **cluster-default + namespace are structural, not convenience.**

The **third (per-pool) tier costs no extra schema** — it is the *same* CRD with `inferencePoolRef` set, in the tenant's namespace, adding override *granularity*, not a new surface. So "multiple levels" is three placements of one object resolved by a small deterministic merge — not three configuration systems.

### Status — surfacing the resolution

The per-pool object's status publishes both *what was resolved* and *what it governs* — so `kubectl get scalingpolicy <name> -o yaml` shows exactly what WVA decided, no unseen algorithm (for pools with a per-pool object; §5 covers default-only pools):

```yaml
status:
  managedVariants:                     # variants WVA manages in this pool (managed set; identified per VA-deprecation proposal)
    - workloadRef: { kind: Deployment, name: granite-premium-prefill }
      role: prefill                    # when known
      cost: 40.0                       # per-replica $/hr (see source below)
    - workloadRef: { kind: Deployment, name: granite-premium-decode }
      role: decode
      cost: 24.0
  effectivePolicy: { … }               # merged result (cluster → ns → per-pool)
  sources: [ … ]                       # contributing tiers, in precedence order
```

- **`effectivePolicy`** (+ **`sources`**) — the merged result and which tier each field came from.
- **`managedVariants`** — the workloads this policy manages, each with `workloadRef`, `role`, and per-replica `cost`. *Which* workloads and *how* they're identified is the VA-deprecation proposal's (§5); this fixes only the **shape**. It is **distinct from pool membership** (an unmanaged pool member wouldn't appear); `cost` is a stable input surfaced so the cost basis of each decision is visible in one place; an **empty list** (no managed workloads) raises `PolicyMatched: False`. Live replica counts stay in the metric — status holds the stable mapping.

## 5. Dependencies and open questions

**Depends on the VA-deprecation proposal — not decided here.** That proposal owns how WVA identifies the workloads it manages and where per-variant `cost` is sourced. This proposal fixes only the **shape** of `managedVariants` (`workloadRef`, `role`, `cost`) and is independent of the mechanism. *(For context: that proposal marks managed objects with the `llm-d.ai/managed` label on the HPA/`ScaledObject` (#1130), which WVA reads — so per-variant `cost` would co-locate there; `cost` comes from the `VariantAutoscaling` CR until then.)*

**Open — where is the effective policy published for a pool with no per-pool object?** `effectivePolicy` / `managedVariants` are shown on the per-pool `ScalingPolicy` status — but per-pool objects are **optional** (a pool governed by the namespace/cluster default alone has none), so today the resolved policy is invisible in exactly the common case. Candidate homes: the `InferencePool` status, a WVA-maintained status-only object per managed pool, or a `wva-config explain <pool>` view. Unresolved.

## 6. Deliberately out of scope (deferred to #1194)

To hold this to the bare minimum, the following are **not** decided here and remain in #1194 for reference:

- Migration path and the `wva-config` CLI / migration tool.
- Deprecation phases and the ConfigMap-removal timeline.
- The full `status` schema beyond what's defined above — the fields `effectivePolicy`, `sources`, `managedVariants` and the `PolicyMatched` condition are in scope; additional conditions, timestamps, and observed generations are deferred.
- Per-role (prefill/decode) configuration — an analyzer-`parameters` concern, not a CRD-shape concern.
- Alternatives considered and the autoscaler comparison matrices.
- The `quota` `parameters` schema and enforcement — owned by #1162.

Once the CRD boundary (§2) and shape (§3) are agreed, these layer on top without changing either.
