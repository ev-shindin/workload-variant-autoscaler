# Proposal: Priority-Weighted Rescale (Redistributive Fair-Share Under Contention)

## Summary

Add an opt-in **rescale** pass to the multi-model optimizer: when GPU demand exceeds the budget, reallocate the **whole budget** by each model's `priority × demand` — reclaiming GPUs from models holding more than their share (lower-priority, even if still hungry) so higher-priority work can run. Today's additive fair-share hands out only *free* GPUs, so once the budget is full `priority` stops mattering and allocation is frozen by arrival order. v1 scopes this to one **accelerator type within a namespace** ([Scope](#scope-competition-axes)); models spanning GPU types are a later phase.

## User story

> A platform operator runs a **production** model and an **internal** model on one fully-booked GPU pool, production at higher `priority`. The internal model scaled up first and holds most of the pool; when production spikes it **can't scale up** — there are no free GPUs to hand out and the internal model is never reclaimed. Production runs hot at its floor while the experiment keeps its GPUs.

`priority` should decide the split when the pool is saturated: each model holds a share proportional to `priority × demand`, reclaiming from lower-priority models when needed, with any unused share flowing back to models with unmet demand — and reclaim never crossing a namespace quota.

## Problem

`GreedyByScoreOptimizer` fair-shares scarce GPUs within a budget from the limiter chain (#1129/#1162), but it is **additive by construction**: driven by `RequiredCapacity = max(0, TotalDemand − TotalSupply)` it can only "add N", never "hold fewer". `fairShareScaleUp` distributes only *unallocated* GPUs and stops at zero; `costAwareScaleDown` runs only on `SpareCapacity > 0`. So once the budget is full, allocation is **frozen by arrival order** and a saturated model is never reclaimed — `priority` only breaks ties while free GPUs last.

*Example:* budget 8 (1 GPU/replica). A (`priority 1`) holds 7 and is saturated; B (`priority 3`) is stuck at its floor of 1 and needs 8. `available == 0`, so B can't grow and A is never reclaimed — the cluster runs almost entirely the low-priority model.

## Goals

- Under contention, allocation reflects **priority**, not arrival order — a higher-priority model can get GPUs even when the budget is full.
- A model needing less than its share yields the remainder to models with unmet demand (no idle GPUs).
- **Opt-in**; unchanged when uncontended. Reclaim never crosses a namespace quota.

## Non-goals

- **Defining the budget** — the limiter chain's job (#1129/#1162); rescale operates within it.
- **Pod-level preemption** — WVA sets replica counts; no eviction or `PriorityClass`.
- **A new optimizer / redefining `priority`** — extends the greedy path; reuses the existing `priority` field (default `1.0`) with a stronger effect under contention.
- **Multi-accelerator models (v1)** — variants spanning GPU types need joint cross-type placement; v1 competes within a single accelerator type ([future scope](#multi-accelerator-models-future-scope)).

## Scope: competition axes

Three axes decide *who competes with whom*; for each, the question is **"is there a per-axis demand signal to weight by?"**

| axis | per-axis demand? | v1 |
|------|------------------|----|
| **namespace** (tenant) | yes — each quota is its own budget (#1162) | **hard partition** — reclaim never crosses |
| **role** (prefill/decode) | yes — disaggregated roles carry distinct demand | **in v1** — role-proportional reclaim (#1237) |
| **accelerator type** | **no** — one accelerator-blind InferencePool per model | **single type only** — multi-type deferred |

So the **v1 unit is a competition group = `(accelerator type, namespace)`**: the models with a variant on that type in that namespace, drawing on the same budget. Groups are independent — reclaim never crosses an accelerator type (GPUs are non-fungible) or a namespace (tenant isolation). This is the user-story case. (A P/D model whose prefill and decode run on *different* accelerator types is itself multi-accelerator, hence deferred; v1 P/D keeps both roles on one type.)

## Solution

Behind a `rescale` flag, when demand exceeds the budget, reallocate the whole budget instead of stopping at the free GPUs.

> **Config.** A ConfigMap flag for now (alongside `priority`), moving to the `ScalingPolicy` CRD with #1194. It governs how a shared budget is split, so its tier follows the budget's scope (cluster-default, or namespace for a quota) — never per-pool.

### Targets (water-filling)

Each model's target is computed from priority and demand, **independent of what it holds** — the move that lets rescale withdraw, not just add. Reserve `minReplicas` floors, then split the rest by `priority × demand`:

```
weight_i = priority_i × demand_i                  (demand = full TotalDemand, not the clamped gap)
floor_i  = minReplicas_i × GPUsPerReplica_i        (the model's reserved GPUs)
share_i  = floor_i + (budget − Σ floor_j) · weight_i / Σ weight_j     (all terms in GPUs)
```

`demand_i` enters the **weight** only as a ratio (units cancel), so it stays in `TotalDemand`'s native token units. A model wanting less than its share is capped at its **demand-in-GPUs** — `TotalDemand` converted to a GPU count via per-replica serving capacity (`TotalDemand ÷ perReplicaCapacity → replicas × GPUsPerReplica`) — or at `maxReplicas`; the freed excess re-splits over the still-hungry models by the same weights, until none exceeds its demand. `budget`, the floors, and the cap are all GPU-valued; targets sum to `min(budget, Σ demand-in-GPUs)`.

Within a group (one accelerator type) replicas are uniform, so quantities are in **GPUs**; conceptually the target is a priority-weighted **capacity** share that equals a GPU share under this homogeneity (the [heterogeneous extension](#multi-accelerator-models-future-scope) generalizes the currency to effective capacity). The action is `target_i − currentGPUs_i` — negative **reclaims**, even from a saturated model, never below `minReplicas`. Targets and actions are in GPUs and **quantize to whole replicas** at each variant's `GPUsPerReplica`; for multi-GPU replicas (LWS, P/D leaders) a sub-replica reclaim rounds down (and may leave the grantee Pending). The "1 GPU/replica" examples here hide this.

**Worked example** — budget 8, 1 GPU/replica, `minReplicas 0`:

| model | priority | demand | weight | share | current | action |
|-------|---------:|-------:|-------:|------:|--------:|-------:|
| A | 1 | 8 | 8 | 2 | 8 | **−6** |
| B | 3 | 8 | 24 | 6 | 0 | **+6** |

A is reclaimed 8→2 (still saturated) to give B 0→6. If B instead demands 4, its share 4.8 caps at 4 and the freed 0.8 flows to A (A 4, B 4).

### Implementation — reuse existing primitives

When `rescale` is on and demand exceeds the budget, compute targets over **every model in the competition group** (including steady ones — reclaim may target a model not asking to scale), then:

1. **Reclaim** (`action < 0`) via `costAwareScaleDown` fed `spare = currentGPUs − target` (most-expensive variant first, respects `minReplicas`); return freed GPUs to `available`. For P/D, shed per role by `roleDemands`.
2. **Fill** (`action > 0`) via the existing `allocateForModel` up to target.

Fairness lives in the target, so the mean-based `fairShareScaleUp` orchestration is **bypassed** (it would re-divide the freed budget and undo the targets). New code: the water-filling target, the reclaim-before-fill order, and the per-role split. Reclaim and grant emit in one reconcile; the grantee's pods may be briefly `Pending` while reclaimed ones drain (the next reconcile converges). Flag off → `Optimize` runs unchanged.

### Multiple budgets — partition by (accelerator type, namespace)

The budget is per accelerator type, and `mergeConstraints` collapses providers to a single per-accelerator minimum, erasing namespace — so under rescale, key the partition by **(accelerator type, namespace)**: each namespace quota (#1162) on each type is its own budget; models without a quota share the cluster budget (#1129) for that type — so reclaim **can** cross namespaces among unquota'd models (they share that budget), while quota'd namespaces stay isolated. A reclaim never crosses a type (non-fungible) or a quota (isolation). Needs the quota limiter to expose constraints by `(namespace, accelerator)`, not pre-merged. Cluster capacity is an outer ceiling: if `Σ quotas > capacity` for a type, the scheduler arbitrates the shortfall (Pending) under an over-budget `Conflict` — a quota is a cap, not a reservation.

## Multi-accelerator models (future scope)

v1 assumes each group is a single accelerator type. A model whose variants span types (A100 + H100 behind one InferencePool) couples groups; four things make it a harder, deferred design:

1. **No per-accelerator demand** — the accelerator-blind router exposes demand only at the pool level, so a model's demand can't be attributed to a type without solving placement jointly.
2. **Non-uniform currency** — an H100 replica ≠ an A100 replica, so targets must be in **effective capacity**, not GPU counts.
3. **Cross-type substitution** — a hungry model may grow on either type and reclaim from either; the cost/availability choice **couples** the per-type budgets into one constrained allocation, not independent passes.
4. **Groups become connected components** over the types a multi-accelerator model spans.

Balancing a model's load across its own accelerators stays the **router's** job, not rescale's. Design intent only; a later phase of this proposal.

## Interactions

- **`minReplicas` is HPA-enforced** — WVA's output is clamped by the `ScaledObject`, so a reclaim below `minReplicas` is ignored (GPUs not freed → grantee Pending). Only `budget − Σ floors` is redistributable; floors exceeding the budget raise a `Conflict`.
- **P/D** — prefill/decode aren't fungible, so reclaim keeps their ratio; fills already do (`allocateByRole`), scale-down is made role-aware by **#1237**. **P/D correctness depends on #1237.**
- **`priority`** — `spec.priority` under #1194; not moved or redefined.

## Observability

A reclaim scales a saturated model down, which looks like a bug unless visible: emit the per-model targets, the priorities/shares behind them, and what was reclaimed — via logs and a `Rescaled` status condition (aligns with #1194's `status.effectivePolicy`). When `Σ minReplicas·GPUsPerReplica > budget`, raise an over-budget `Conflict` stating the shortfall.

## Implementation phases

1. **Reclaim/fill to target** behind the flag, single budget (reuses `costAwareScaleDown` + `allocateForModel`; role-proportional reclaim depends on #1237). Off by default.
2. **Observability + stability** — `Rescaled` condition; hysteresis against oscillation.
3. **Multiple budgets** — partition by `(accelerator type, namespace)` under the cluster ceiling (#1162).
4. **Multi-accelerator** — heterogeneous groups in effective-capacity units (future scope).

## Alternatives

1. **Raise the budget** — moot when GPUs are physically capped.
2. **Strict priority** (highest takes all, then next) — starves low-priority models to their floor; proportional shares give everyone a priority-weighted slice. Could be a selectable share function later.
3. **Pod-level preemption** (Kueue / `PriorityClass`) — orthogonal; preempts after WVA sets counts and can't express "run fewer replicas of A so B can scale". Rescale stays within Kueue's admitted budget.
4. **Cost-based reclaim order** — reuses the existing most-expensive-first rule; a refinement later.

## Open questions

- **Oscillation** — what hysteresis (a minimum share-gap, or a cool-down) prevents reclaim↔re-grant churn across reconciles?
