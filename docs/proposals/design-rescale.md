# Proposal: Priority-Weighted Rescale (Redistributive Fair-Share Under Contention)

## Summary

Add an opt-in **rescale** pass to the multi-model optimizer: when GPU demand
exceeds the budget, reallocate the **whole budget** in proportion to each model's
priority × demand — reclaiming GPUs from models holding more than their share
(typically lower-priority, even if still hungry) so a higher-priority model can
run. Today's additive fair-share only hands out *unallocated* GPUs, so once the
budget is exhausted `priority` stops mattering and allocation is frozen by arrival
order.

## User Story

> As a platform operator, I run a **production model** (live user traffic) and an
> **internal-user model** (experimentation and tooling) on one **fully-booked GPU
> pool**, with production at higher `priority`. The internal model scaled up first
> and holds most of the GPUs; when production traffic spikes, production **can't
> scale up to meet it** — the optimizer only hands out *free* GPUs (there are none)
> and never reclaims the internal model's. Production runs hot at its floor, latency
> climbing, while the internal experiment keeps its GPUs.

**What we are trying to achieve:** make `priority` actually decide the GPU split
when the cluster is saturated. Instead of "whoever scaled up first keeps the GPUs,"
each model should hold a share of the budget proportional to its priority and
demand — reclaiming from lower-priority models when needed — so the most important
work runs even when there is nothing free to hand out. A model that needs less than
its share gives the remainder back, and reclaim never crosses a tenant's namespace
quota.

## Problem Statement

The `GreedyByScoreOptimizer` (`internal/engines/pipeline/greedy_score_optimizer.go`)
fair-shares scarce GPUs within a budget from the limiter chain (#1129 / #1162). It
is **additive by construction**: driven by
`RequiredCapacity = max(0, TotalDemand − TotalSupply)` (the incremental gap,
clamped at 0), it can only say "add N", never "hold fewer". `fairShareScaleUp`
distributes only *unallocated* GPUs and stops at `totalGPUs == 0`;
`costAwareScaleDown` runs only when a model has `SpareCapacity > 0`.

So once the budget is full, allocation is **frozen by arrival order** and a
**saturated model is never reclaimed** — even for a higher-priority model starved
for exactly those GPUs. `priority` only breaks ties while free GPUs last.

*Illustration:* budget 8 GPUs (1 GPU/replica). Model A (`priority 1`) holds 7 and
is saturated; Model B (`priority 3`) is stuck at its floor of 1 and needs 8.
`available == 0`, so B can't scale up and A is never reclaimed → the cluster runs
almost entirely the low-priority model, with no in-product way to fix it.

## Goals

- Under contention, allocation reflects **priority**, not arrival order.
- A higher-priority model can get GPUs **even when the budget is already full**.
- A model needing less than its share yields the remainder to models with **unmet
  demand** — no idle GPUs.
- **Opt-in**; unchanged when there is no contention.

## Non-Goals

- **Defining the budget** — that is the limiter chain's job (#1129 / #1162);
  rescale operates within it and never moves budget across namespaces (tenant
  isolation).
- **Pod-level preemption** — WVA sets replica counts; the HPA/scheduler enact
  them. No pod eviction or `PriorityClass`.
- **A new optimizer** — rescale extends the existing greedy path.
- **Redefining `priority`** — reuses the existing field (default `1.0`) with a
  stronger effect under contention.

## Proposed Solution

Gate rescale behind a config flag (`rescale`). When demand exceeds the budget,
reallocate the whole budget instead of stopping at the free GPUs.

> **Config surface.** A ConfigMap flag for now (alongside `priority`), migrating
> to the `ScalingPolicy` CRD with #1194. It governs how a *shared* budget is
> split, so it cannot be per-pool; its tier follows the budget's scope
> (cluster-default, or namespace-default for a quota).

### Targets (water-filling)

Each model's target is computed from priority and demand, **independent of what it
holds** — the move that lets rescale withdraw, not just add. Reserve `minReplicas`
floors first, then split the rest by `priority × total demand`:

```
weight_i = priority_i × demand_i              (demand = TotalDemand, not the clamped gap)
share_i  = minReplicas_i + (budget − Σ minReplicas_j) · weight_i / Σ weight_j
```

A model wanting less than its share is capped at its demand (or `maxReplicas`); the
freed excess is re-split over the still-hungry models by the same weights,
repeating until none exceeds its demand. The result is each model's **final
target** (≤ demand; targets sum to `min(budget, Σ demand)`); a hungry model can
rise **above** its first-pass share by absorbing a capped model's surplus.

All quantities are in **GPUs**; `demand` enters only as a weight, so its units
cancel. The action is `target_i − currentGPUs_i` — positive adds, negative
**reclaims** (even from a saturated model), never below `minReplicas`.

#### Worked example

Budget 8, 1 GPU/replica, `minReplicas: 0`:

| model | priority | demand | weight | share | current | action |
|-------|---------:|-------:|-------:|------:|--------:|-------:|
| A     | 1        | 8      | 8      | 2     | 8       | **−6** |
| B     | 3        | 8      | 24     | 6     | 0       | **+6** |

A is reclaimed 8→2 (still saturated) to give B 0→6. If instead **B demands 4**:
B's share 4.8 is capped at 4, and the freed 0.8 flows to A → A 4, B 4 (surplus not
wasted).

### Implementation — reuse existing primitives

When `rescale` is on and demand exceeds the budget, compute targets over **every
model drawing on the budget** (including steady ones — reclaim may target a model
not asking to scale), then:

1. **Reclaim first** (`action < 0`) with `costAwareScaleDown`, fed
   `spare = currentGPUs − target` (most-expensive variant first, respects
   `minReplicas`); return freed GPUs to `available`. For disaggregated models, shed
   **per role** by `roleDemands` to keep the P/D ratio.
2. **Fill** (`action > 0`) with the existing per-model allocator `allocateForModel`
   up to the target.

Fairness lives in the target, so the mean-based `fairShareScaleUp` orchestration is
**not** used (it would re-divide the freed budget and undo the targets). New code:
the water-filling target, the reclaim-before-fill reorder, and (for P/D) the
per-role target split. Off → `Optimize` is unchanged.

Reclaim and grant are emitted in one reconcile; the grantee's pods may be briefly
`Pending` while the reclaimed ones drain — an accepted transient the scheduler
sequences (the next reconcile converges).

### Multiple budgets — partition by scope

The computation above assumes one shared budget. `mergeConstraints` collapses
providers to a single per-accelerator minimum, erasing the namespace dimension —
so under rescale, **partition by namespace** instead: each namespace quota (#1162)
is its own budget; models without one share the cluster budget (#1129). Targets
run per partition, so a reclaim **never crosses a quota boundary** (tenant
isolation).

Cluster capacity is an **outer ceiling**: if `Σ quotas > capacity`, rescale honors
each namespace's result and lets the scheduler arbitrate the shortfall (Pending
pods) under an over-budget `Conflict` — a quota is a cap, not a reservation. This
needs the quota limiter to expose constraints **by namespace** (not pre-merged).

## Interaction With Existing Concepts

- **`priority`** — already in policy (default `1.0`); rescale makes it set the
  proportional share under contention, not just fill order. `spec.priority` under
  #1194; not moved or redefined.
- **`minReplicas` is HPA-enforced** — WVA's output is clamped to
  `[minReplicas, maxReplicas]` by the `ScaledObject`, so a reclaim below
  `minReplicas` is ignored (GPUs not freed → grantee Pending). Only
  `budget − Σ floors` is redistributable; if the floors themselves exceed the
  budget, that is an over-commitment rescale cannot fix (it raises a `Conflict` —
  see Observability).
- **Disaggregated (P/D)** — prefill and decode are not fungible, so reclaim must
  keep their ratio. Fills already do (`allocateByRole`); scale-down is made
  role-aware by **#1237**. Rescale splits the target by `roleDemands` and feeds
  per-role reclaim amounts, reusing that fix. **P/D correctness depends on #1237.**

## Observability

A reclaim scales a saturated model down, which looks like a bug unless it is
visible. Emit the per-model targets, the priorities/shares behind them, and what
was reclaimed — via logs and a `Rescaled` status condition (self-contained; aligns
with #1194's `status.effectivePolicy` later). When
`Σ minReplicas·GPUsPerReplica > budget`, raise an over-budget `Conflict` stating
the shortfall rather than leaving unexplained Pending pods.

## Implementation Phases

1. **Reclaim/fill to target** behind the flag, over a single budget (reuses
   `costAwareScaleDown` + `allocateForModel`; role-proportional reclaim depends on
   #1237). Off by default; log targets/reclaims.
2. **Observability + stability** — `Rescaled` condition; hysteresis against
   oscillation.
3. **Multiple budgets** — per-namespace partitioning under the cluster ceiling
   (composes with #1162).

## What Does NOT Change

- The uncontended / flag-off path — `Optimize` runs byte-for-byte as today.
- `costAwareScaleDown` and `allocateForModel` are called as-is
  (`costAwareScaleDown` made role-aware by #1237, not by rescale);
  `fairShareScaleUp`'s orchestration is bypassed, not modified.
- The pipeline shape, the `ScalingOptimizer` interface, the budget source
  (#1129 / #1162), and the meaning of `priority` / `minReplicas` / `maxReplicas` /
  scale-to-zero.

## Alternatives Considered

1. **Raise the budget** — moot when GPUs are physically capped.
2. **Strict priority** (highest takes all it wants, then the next) — starves a
   low-priority model down to its `minReplicas` floor; proportional shares instead
   give every model a priority-weighted slice. Could be a selectable share function
   later.
3. **Pod-level preemption** (Kueue / `PriorityClass`) — orthogonal; preempts at the
   scheduler after WVA sets counts and cannot express "run fewer replicas of A so B
   can scale". Rescale must stay within Kueue's admitted budget.
4. **Cost-based reclaim order** — reuses the existing most-expensive-first rule; a
   cost-aware order is a later refinement.

## Open Questions

- **Oscillation** — what hysteresis (a minimum share-gap, or a cool-down) prevents
  reclaim↔re-grant churn across reconciles?
