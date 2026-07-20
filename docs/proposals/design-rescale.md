# Proposal: Priority-Weighted Rescale (Redistributive Fair-Share Under Contention)

## Summary

Add an opt-in **rescale** pass to the multi-model optimizer: when GPU demand exceeds the budget, reallocate the **whole budget** by each model's `priority × demand` — reclaiming GPUs from models holding more than their share (lower-priority, even if still hungry) so higher-priority work can run. Today's additive fair-share hands out only *free* GPUs, so once the budget is full `priority` stops mattering and allocation is frozen by arrival order. v1 scopes this to one **accelerator type within a namespace** ([Scope](#scope-competition-axes)); models spanning GPU types are a later phase.

Because a reclaim is a **scale-down**, rescale is only effective when WVA can actuate scale-downs *promptly and coherently*. On today's metric→HPA path WVA cannot — the downstream HPA's scale-down stabilization (k8s default 300 s) holds the reclaim while the fill actuates fast, running the group over-budget. So rescale **depends on** in-WVA stabilization (#1353) and steady-state direct actuation, and must be *integrated* with them rather than layered on top ([Dependencies & sequencing](#dependencies--sequencing)).

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
- **Bounded, non-oscillating convergence** — a reclaim→re-grant never runs the group persistently over-budget, and does not churn across reconciles ([Convergence & stability](#convergence--stability)).

## Non-goals

- **Defining the budget** — the limiter chain's job (#1129/#1162); rescale operates within it.
- **Pod-level preemption** — WVA sets replica counts; no eviction or `PriorityClass`.
- **A new optimizer / redefining `priority`** — extends the greedy **V2** path; reuses the existing `priority` field (default `1.0`) with a stronger effect under contention. (The V1 saturation path is out of scope — see [Interactions](#interactions).)
- **Multi-accelerator models (v1)** — variants spanning GPU types need joint cross-type placement; v1 competes within a single accelerator type ([future scope](#multi-accelerator-models-future-scope)).
- **Owning the stabilization/actuation machinery** — rescale consumes #1353 and direct actuation; it does not reimplement them.

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

Within a group (one accelerator type) replicas are uniform, so quantities are in **GPUs**; conceptually the target is a priority-weighted **capacity** share that equals a GPU share under this homogeneity (the [heterogeneous extension](#multi-accelerator-models-future-scope) generalizes the currency to effective capacity). The action is `target_i − currentGPUs_i` — negative **reclaims**, even from a saturated model, never below `minReplicas`.

**Quantization.** Targets and actions are in GPUs and **quantize to whole replicas** at each variant's `GPUsPerReplica`. For multi-GPU replicas (LWS, P/D leaders) a sub-replica reclaim rounds down: a model can only shed a *whole* replica, so a group whose members each hold `k` GPUs/replica frees GPUs in steps of `k`. If the reclaimed model cannot shed a whole replica (its share rounds within one replica of what it holds) **no GPUs free that cycle** and the grantee stays Pending — a stall, not just a delay, and one that must be surfaced in observability. *Worked example:* budget 8, `GPUsPerReplica = 2` (4 replicas). A holds 3 replicas (6 GPUs), B holds 1 (2). Water-filling A→4 GPUs, B→4 GPUs = 2 replicas each — A sheds 1 replica (−2), B gains 1 (+2), exact. Had the split been A→5/B→3 GPUs, neither is a replica multiple; A rounds down to 4 (sheds 1 replica), B takes the whole freed 2 → A=4, B=4 again. The "1 GPU/replica" examples below hide this rounding.

**Worked example** — budget 8, 1 GPU/replica, `minReplicas 0`:

| model | priority | demand | weight | share | current | action |
|-------|---------:|-------:|-------:|------:|--------:|-------:|
| A | 1 | 8 | 8 | 2 | 8 | **−6** |
| B | 3 | 8 | 24 | 6 | 0 | **+6** |

A is reclaimed 8→2 (still saturated) to give B 0→6. If B instead demands 4, its share 4.8 caps at 4 and the freed 0.8 flows to A (A 4, B 4).

### Implementation — reuse existing primitives, respect the release lag

When `rescale` is on and demand exceeds the budget, compute targets over **every model in the competition group** (including steady ones — reclaim may target a model not asking to scale). Two properties of the current engine shape *how* the reclaim and fill are applied:

- **Reclaimed GPUs do not free in the same cycle.** Usage is derived from last-actuated `CurrentReplicas` (`status.replicas`), and scale-down freed replicas are not re-injected into the in-cycle `available` budget; freed capacity only appears next cycle once actuation drops `status.replicas`. So the naive "reclaim → return freed GPUs to `available` → fill to target in one pass" would over-count the budget.
- **The fill must not out-run the reclaim.** Granting a model up to `target` before the reclaimed GPUs are physically free over-subscribes the group and leaves the grantee Pending for as long as the reclaim takes to actuate.

Rescale therefore applies the transfer as a **paced, feedback-driven convergence**, not a one-shot:

1. **Reclaim** (`action < 0`) via `costAwareScaleDown` fed `spare = currentGPUs − target` (most-expensive variant first, respects `minReplicas`). For P/D, shed per role by `roleDemands` (#1237). Reclaims are emitted **immediately** and marked so downstream stabilization does not hold them ([Interactions](#interactions)).
2. **Fill** (`action > 0`) via the existing `allocateForModel`, but only up to **`min(target, GPUs actually free this cycle)`** — the free GPUs observed from the current `available`/`availableByNS`, which grows as prior-cycle reclaims actuate. The remaining fill lands on subsequent cycles as capacity frees, so the group is **never** transiently over-budget.

The **target** is *not* a separate allocator bolted beside `fairShareScaleUp` — it is the same priority-weighted proportional split the fair-share already performs, generalized to redistribute the whole budget ([Composition](#composition--one-allocator-floor-as-the-parameter)). Only the **application** of the target is rescale-specific: the reclaim-before-fill ordering, the per-role split, and the free-capacity-gated fill. Flag off → `Optimize` runs unchanged.

Convergence latency is therefore ≈ *reclaim actuation time* + 1–2 optimize cycles (the loop runs on a fixed 30 s cadence). This is inherent to the metric/replica feedback loop; rescale makes it bounded and monotone rather than racing the reclaim.

### Composition — one allocator, floor as the parameter

Rescale should **generalize** the existing fair-share, not run beside it. `fairShareScaleUp` already splits the budget in proportion to `priority × demand` — its per-round mean is taken over the priority-weighted remaining demand, so priority already governs the split. It is *additive* not because of the weighting but because of **two floors**: it seeds the loop with only the **free** GPUs (`available`), and implicitly floors every model at **what it currently holds** (it can add, never withdraw). Once free GPUs reach zero the loop stops — which is exactly why priority "stops mattering when the pool is full."

Rescale changes **one parameter — the floor.** Lower each model's floor from `currentGPUs_i` to `minReplicas_i · GPUsPerReplica_i`, run the *same* proportional split over the **whole** group budget (not just the free remainder), and the result is the water-filling target — a model can now be brought *below* what it holds:

| mode | budget seeded | per-model floor | effect |
|------|---------------|-----------------|--------|
| **additive** (today / rescale off) | free GPUs (`available`) | `currentGPUs_i` | add only; frozen when free = 0 |
| **redistributive** (rescale on) | whole group budget | `minReplicas_i · GPUsPerReplica_i` | withdraw down to the floor |

So this is **one allocator** parameterized by the floor (and the budget it spans), not an `if rescale { … }` fork that bypasses `fairShareScaleUp`. The invariants that must hold in both modes — namespace/quota caps, `maxReplicas`, P/D role ratios, whole-replica quantization — then live in a single place and cannot drift between two paths. The `rescale` flag selects the floor; it does not select a second code path. (The redistributive target can be *negative* — a reclaim — which is why only its *application* differs; see above.)

### Dependencies & sequencing

A reclaim is a scale-down, and its usefulness is bounded by how fast WVA can *actuate* a scale-down:

- **Today** WVA only emits a `wva_desired_replicas` metric; an operator-owned HPA/KEDA does the `/scale` patch. WVA authors no damping, but the **downstream HPA does**: with the k8s default `behavior` a scale-down is held by a **300 s stabilization window** while a scale-up is immediate. Under that default a rescale reclaim is held 300 s while the fill actuates at once — the group runs over-budget for the whole window and rescale delivers no benefit.
- **#1353 (in-WVA stabilization)** moves that damping into WVA (default 300 s scale-down window). Without integration ([Interactions](#interactions)) it would damp the reclaim the same way.
- **Steady-state direct actuation** — extending the existing `DirectActuator` (today wired only for 0→1 scale-from-zero) to the 1→N range — is what lets WVA, not a downstream 300 s window, own scale-down timing.

**Recommended order:** (1) #1353 + the HPA-near-passthrough contract; (2) steady-state direct actuation; (3) this proposal, integrated with both. Until (1)+(2), rescale must be gated behind **`HPA scaleDown.stabilizationWindowSeconds = 0`** (passthrough), and even then tolerates a brief pod-lifecycle-bounded over-subscription rather than a 300 s one. Shipping rescale on the default-HPA path makes it inert under the very contention it targets.

### Multiple budgets — partition by (accelerator type, namespace)

The budget is per accelerator type, and `mergeConstraints` collapses providers to a single per-accelerator minimum, erasing namespace — so under rescale, key the partition by **(accelerator type, namespace)**: each namespace quota (#1162) on each type is its own budget; models without a quota share the cluster budget (#1129) for that type — so reclaim **can** cross namespaces among unquota'd models (they share that budget), while quota'd namespaces stay isolated. A reclaim never crosses a type (non-fungible) or a quota (isolation).

**Plumbing status.** The per-(namespace, type) view **already exists** for a namespace-scoped quota — `QuotaInventory.NamespaceResourcePools` → `ResourceConstraints.NamespacePools` → `availableByNS` (`mergeNamespaceConstraints`) — and rescale should consume `availableByNS` directly rather than the collapsed `available`. It does **not** exist for **physical inventory** (#1129) or a **cluster-scope** quota, which carry no namespace dimension; partitioning those by namespace, and respecting physical capacity *and* quota simultaneously, needs the physical∧quota composition tracked in **#1003** (a hard prerequisite for that case). Cluster capacity is an outer ceiling: if `Σ quotas > capacity` for a type, the scheduler arbitrates the shortfall (Pending) under an over-budget `Conflict` — a quota is a cap, not a reservation.

## Multi-accelerator models (future scope)

v1 assumes each group is a single accelerator type. A model whose variants span types (A100 + H100 behind one InferencePool) couples groups; four things make it a harder, deferred design:

1. **No per-accelerator demand** — the accelerator-blind router exposes demand only at the pool level, so a model's demand can't be attributed to a type without solving placement jointly.
2. **Non-uniform currency** — an H100 replica ≠ an A100 replica, so targets must be in **effective capacity**, not GPU counts.
3. **Cross-type substitution** — a hungry model may grow on either type and reclaim from either; the cost/availability choice **couples** the per-type budgets into one constrained allocation, not independent passes.
4. **Groups become connected components** over the types a multi-accelerator model spans.

Balancing a model's load across its own accelerators stays the **router's** job, not rescale's. Design intent only; a later phase of this proposal.

## Interactions

- **Stabilization (#1353) — the reclaim must not be damped as if it were noise.** #1353 stabilizes each variant independently with a scale-*down* trailing window (default 300 s) whose purpose is to absorb *flappy demand drops*. A rescale reclaim is a **deliberate, priority-driven** reallocation, not noise, and it is **coupled** to a fill on another variant — damping the two independently lets the fill through while holding the reclaim, breaking rescale's `Σ targets = budget` invariant and over-subscribing the group. Contract: (a) reclaim decisions carry a reason (e.g. `DecisionReasonRescale`) that **bypasses the trailing scale-down window** (still honoring hard min floors and, if configured, rate policies); and (b) for a group actively under rescale, WVA does **not** stack #1353's trailing-window stabilization on top of the water-filling target — rescale's target *is* the stabilized decision for that group, and its own hysteresis ([Convergence & stability](#convergence--stability)) prevents churn. Uncontended groups keep #1353 unchanged.
- **Actuation & sequencing** — see [Dependencies & sequencing](#dependencies--sequencing). Rescale requires WVA-owned prompt scale-down (direct actuation) or, on the metric path, an HPA passthrough (`scaleDown.stabilizationWindowSeconds = 0`).
- **`minReplicas`** is a floor enforced in more than one place, and the values must agree. The V2 optimizer does **not** raise a scale-up to `minReplicas` — that floor is enforced by the operator's HPA/`ScaledObject` (`minReplicas`) and, weakly, by the WVA enforcer's floor-of-1. Rescale reserves `floor_i = minReplicas·GPUsPerReplica` and redistributes only `budget − Σ floors`. **Invariant:** the HPA `minReplicas` must equal WVA's assumed floor — if the HPA floor is higher, a reclaim below it is silently ignored, the GPUs never free, and the grantee is *permanently* Pending. Read the floor from one source (the effective `ScalingPolicy`, #1194). Floors exceeding the budget raise a `Conflict`.
- **V1 saturation path** raises `TargetReplicas` up to `minReplicas` even under scarcity, which would directly contradict a reclaim. Rescale is therefore **scoped to the V2 greedy path**; the V1 min-raise divergence is out of scope (and a candidate for reconciliation independently).
- **P/D** — prefill/decode aren't fungible, so reclaim keeps their ratio; fills already do (`allocateByRole`), scale-down is made role-aware by **#1237**. **P/D correctness depends on #1237.**
- **`priority`** — `spec.priority` under #1194; not moved or redefined.

## Convergence & stability

Two distinct dynamics, addressed separately:

- **In-flight reclaim (not oscillation).** While a reclaim is actuating, usage still reflects the pre-reclaim `status.replicas`, so the next cycle recomputes the *same* (stable) target and re-emits the reclaim — it does not oscillate, it holds until the reclaim drains. The free-capacity-gated fill (Solution §2) ensures the group is never over-budget during this hold.
- **Threshold oscillation.** When demand hovers around the contention boundary, rescale can toggle on/off, churning reclaim↔re-grant across cycles. Rescale **owns** its hysteresis for contended groups (it does not stack a second trailing window on #1353): apply a minimum share-gap (only reclaim when a model's share deficit exceeds a threshold) and/or a cool-down (a model recently reclaimed is not re-granted for N cycles). This is the mechanism referenced by the [stabilization interaction](#interactions).

## Observability

A reclaim scales a saturated model down, which looks like a bug unless visible: emit the per-model targets, the priorities/shares behind them, and what was reclaimed — via logs and a `Rescaled` status condition (aligns with #1194's `status.effectivePolicy`). Make the transfer legible as one event ("reclaim N from A → grant to B"), keep it consistent with #1353's `stabilization-decision` log (a held/in-flight reclaim must not read as a stuck optimizer), and surface a signal for "grantee Pending pending an in-flight reclaim" (including the quantization stall) so operators observe the transient rather than page on it. When `Σ minReplicas·GPUsPerReplica > budget`, raise an over-budget `Conflict` stating the shortfall.

## Implementation phases

1. **Reclaim/fill to target** behind the flag, single budget, **V2 only**, with the free-capacity-gated fill and the reclaim-bypasses-scale-down-damping contract (reuses `costAwareScaleDown` + `allocateForModel`; role-proportional reclaim depends on #1237). Off by default; requires HPA passthrough or direct actuation.
2. **Observability + stability** — `Rescaled` condition; rescale-owned hysteresis (min share-gap / cool-down) against threshold oscillation.
3. **Multiple budgets** — partition by `(accelerator type, namespace)` via `availableByNS` under the cluster ceiling (#1162); physical∧quota composition depends on **#1003**.
4. **Multi-accelerator** — heterogeneous groups in effective-capacity units (future scope).

## Alternatives

1. **Raise the budget** — moot when GPUs are physically capped.
2. **Strict priority** (highest takes all, then next) — starves low-priority models to their floor; proportional shares give everyone a priority-weighted slice. Could be a selectable share function later.
3. **Pod-level preemption** (scheduler `PriorityClass` / Kueue) — cannot own this. Preemption evicts a *pod*, but the low-priority model's **desired replica count is unchanged**, so its own controller (Deployment/ScaledObject) immediately recreates the evicted pod — the reallocation never sticks. Only lowering the model's replica *count* reclaims its share, and setting replica counts is the optimizer's job, not the scheduler's. Preemption is therefore orthogonal (it arbitrates pods within an already-set count); rescale stays within Kueue's admitted budget.
4. **Cost-based reclaim order** — reuses the existing most-expensive-first rule; a refinement later.
5. **Rely on the HPA's own scale-down damping** — rejected: it is per-variant and unaware of the reclaim↔fill coupling, so it holds the reclaim while the fill actuates (the failure mode this proposal is designed around).

## Open questions

- **Hysteresis tuning** — what minimum share-gap and/or cool-down best prevents threshold oscillation without making reclaim sluggish under genuine, sustained priority contention? (Proposed default: reclaim only when the share deficit ≥ one replica *and* the model was not reclaimed within the last N=2 cycles.)
- **Direct-actuation dependency** — should phase 1 hard-block on steady-state direct actuation, or ship gated behind an HPA-passthrough precondition with a clear operator warning? (This doc recommends the latter as an interim, the former as the target state.)
