# Proposal: ScalingPolicy CRD — minimal core

## Scope

This is the **bare-minimum** core of the WVA configuration CRD, split out from the broader analysis in **#1194** (kept open for reference). Per review, it answers only the two questions that gate the design:

1. **What is the relationship between the Config CRD and the HPA object?** (§2)
2. **What is the generic shape (schema) of the CRD?** (§3)

Everything else from #1194 — migration tooling, deprecation phases, the full status-condition catalog, per-role detail, alternatives, comparison matrices — is **deliberately deferred** (§5). The goal here is to agree the CRD's *boundary* and *shape* first; the rest layers on top without changing either.

## 1. The object

One namespaced CRD, `ScalingPolicy` (`scaling.llm-d.ai/v1alpha1`), carries WVA's scaling configuration. It is matched to a workload through that workload's **`InferencePool`** (defined by the Gateway API Inference Extension) — not through the workload, and not through its HPA:

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

**They are decoupled and own different things. WVA does not replace, write to, or reference the HPA.**

| | Owns | Reads | Writes |
|---|---|---|---|
| **HPA / KEDA `ScaledObject`** (tenant-owned) | replica **actuation** | the `wva_desired_replicas` metric | the workload's `spec.replicas` |
| **WVA controller** | the scaling **decision** | `ScalingPolicy` + cluster/pool state | the `wva_desired_replicas` metric (+ `ScalingPolicy.status`) |
| **`ScalingPolicy` CRD** | the scaling **policy** (priority, thresholds, scale-to-zero, quota) | — | — |

The data flow is one-directional:

```
ScalingPolicy ──configures──▶ WVA ──emits──▶ wva_desired_replicas ──read by──▶ HPA / ScaledObject ──sets──▶ spec.replicas
```

The consequences answer the question directly:

- **No cross-reference, either way.** The HPA / `ScaledObject` carries **no** WVA-specific field, and `ScalingPolicy` references the `InferencePool`, **not** the HPA. This is the entire point of a single surface: adding WVA config must not mean editing every tenant's HPA. (An `HPA → ScalingPolicy` reference would put a WVA-specific field back on every workload — exactly what this avoids.)
- **WVA is read-only on the HPA / `ScaledObject`.** It never writes `spec.replicas` directly; it only publishes the metric the HPA already consumes. Tenant-owned objects stay tenant-owned.
- **HPA `maxReplicas` stays, as a complementary hard ceiling.** `ScalingPolicy` does not replace it: WVA's desired count is clamped by the HPA exactly as today. `ScalingPolicy` sets the *policy* WVA decides within; `maxReplicas` is the tenant's final per-workload safety cap.
- **Scale-to-zero is a policy toggle, actuated by the HPA layer.** `spec.scaleToZero.enabled` lets WVA emit a desired of 0; KEDA/HPA actuate it. WVA decides 0; it does not force it.

In short: **`ScalingPolicy` configures the decision; the HPA actuates it.** The CRD sits *upstream* of the metric the HPA already reads, so it slots into the existing HPA/KEDA flow without modifying it.

## 3. The generic schema shape (review question 2)

The CRD is a **thin typed envelope around pluggable, schemaless plugin lists** — so the schema itself is small and fixed, and new analyzers/limiters never change it. Two validation tiers, deliberately:

```yaml
spec:
  # (a) typed, cross-cutting fields — OpenAPI/CEL-validated, `kubectl explain`-able
  inferencePoolRef: { name: granite-premium-pool }   # match key (optional; see §4)
  priority: 2.0
  scaleToZero: { enabled: false }

  # (b) pluggable lists — {type, name, parameters}; `parameters` is plugin-owned
  analyzers:                          # one or more; tenant-tunable
    - type: saturation
      name: sat                       # identity, used for cross-tier merge
      parameters:                     # x-kubernetes-preserve-unknown-fields
        scaleUpThreshold: 0.95
  limiters:                           # zero or more; compose as a chain
    - type: gpu-inventory
    - type: quota                     # parameters schema owned by #1162
```

- **(a) Stable cross-cutting fields** (`priority`, `scaleToZero`, `inferencePoolRef`) are **typed** — OpenAPI/CEL-validated, discoverable via `kubectl explain`.
- **(b) Plugin lists** (`analyzers`, `limiters`) are name-keyed lists of `{type, name, parameters}` where **`parameters` is `x-kubernetes-preserve-unknown-fields`** — each plugin parses and validates its own parameters at load. This is the **EPP `EndpointPickerConfig` pattern** (llm-d's inference scheduler already configures plugins this way), reused for *shape* only — there is no shared config between EPP and WVA; both merely read the `InferencePool`.

The one property that makes the shape "generic": **adding an analyzer or limiter ships its own `parameters` keys and the CRD schema does not change.** The optimizer is a single fixed stage with nothing to configure today; the schema leaves room for a `spec.optimizer` block later with no redesign.

(The `quota` limiter's `parameters` schema and its cluster-scope enforcement are owned by **#1162** / issue #1002 — quota lives on this surface, but its shape is settled there, not here.)

## 4. Why more than one tier (the quota constraint)

The surface is still **one CRD**. It is resolved at up to **three placements** of that same object — not three schemas and not three kinds — by a deterministic lookup in fixed order:

1. **Cluster-default** — `ScalingPolicy` in the system namespace, no `inferencePoolRef`.
2. **Namespace-default** — `ScalingPolicy` in the workload's namespace, no `inferencePoolRef`.
3. **Per-pool** — `ScalingPolicy` in the workload's namespace, `inferencePoolRef` set.

Fields merge cluster → namespace → per-pool (higher tier wins; `analyzers` merge by `name`).

**This cannot collapse to a single tier, because quota forces at least two — by ownership, not preference:**

- **Quota is cluster-admin-owned and cluster-scoped.** A per-namespace cap is a cluster-admin decision a tenant must not be able to raise, and a cluster *aggregate* needs a cluster-wide view (independent per-namespace instances each see full node inventory and would collectively overcommit). So quota must live on a cluster-admin-owned, system-namespace object.
- **Thresholds / `priority` / `scaleToZero` are tenant-owned and namespace-scoped.** A tenant tunes these for their own pools.

These two have **different owners and different RBAC**; they cannot share one object without either letting tenants edit quota or letting admins own every tenant's thresholds. So **cluster-default + namespace are structurally required** — independent of any convenience argument.

The **third (per-pool) tier costs no extra schema**: it is the *same* CRD with `inferencePoolRef` set, in the tenant's own namespace. It adds override *granularity* (one pool tuned differently from its namespace default), not a new surface or a new kind. So "multiple levels" here means three placements of one object resolved by a small, deterministic merge — not three configuration systems.

> The merged result is published on the per-pool object's `status.effectivePolicy` (with `status.sources`), so `kubectl get scalingpolicy <name> -o yaml` shows exactly what is in force and which tiers produced it — the resolution is observable, not an unseen algorithm. (Status field detail deferred to #1194.)

## 5. Deliberately out of scope (deferred to #1194)

To hold this to the bare minimum, the following are **not** decided here and remain in #1194 for reference:

- Migration path and the `wva-config` CLI / migration tool.
- Deprecation phases and the ConfigMap-removal timeline.
- The full `status` condition catalog (only `effectivePolicy` / `sources` are referenced above).
- Per-role (prefill/decode) configuration — an analyzer-`parameters` concern, not a CRD-shape concern.
- Alternatives considered and the autoscaler comparison matrices.
- The `quota` `parameters` schema and enforcement — owned by #1162.

Once the CRD boundary (§2) and shape (§3) are agreed, these layer on top without changing either.
