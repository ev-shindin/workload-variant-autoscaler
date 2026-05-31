# Scaling Optimizers

## Overview

A **scaling optimizer** is the pipeline stage that turns an analyzer's capacity
signals into concrete per-variant replica targets. Analyzers answer *"how much
capacity does this model need?"*; optimizers answer *"which variants should run
how many replicas to satisfy that need?"*.

Optimizers sit between the analyzer and the enforcer in the V2 scaling pipeline:

```
Analyzer  ──▶  Optimizer  ──▶  Enforcer  ──▶  decisions applied
(capacity      (replica        (scale-to-zero
 signals)       targets)        & min-replica policy)
```

The autoscaler ships two optimizers, selected automatically based on whether GPU
capacity is constrained:

| Optimizer | Identifier | Mode | When selected |
|-----------|------------|------|---------------|
| `CostAwareOptimizer` | `cost-aware` | Unlimited | `enableLimiter: false` (default) |
| `GreedyByScoreOptimizer` | `greedy-by-score` | Limited | `enableLimiter: true` |

Both implement the `ScalingOptimizer` interface and are stateless, so the engine
can swap between them on each reconcile based on configuration.

> **Scope:** Optimizers are used by the V2 saturation analyzer
> (`analyzerName: saturation`) and the queueing-model analyzer. The legacy V1
> percentage-based path uses its own target-building flow and does not go through
> these optimizers.

## Table of Contents

- [Overview](#overview)
- [The ScalingOptimizer interface](#the-scalingoptimizer-interface)
- [Inputs and outputs](#inputs-and-outputs)
- [Optimizer selection](#optimizer-selection)
- [CostAwareOptimizer (unlimited mode)](#costawareoptimizer-unlimited-mode)
  - [Scale-up](#cost-aware-scale-up)
  - [Scale-down](#cost-aware-scale-down)
- [GreedyByScoreOptimizer (limited mode)](#greedybyscoreoptimizer-limited-mode)
  - [Resource constraints](#resource-constraints)
  - [Fair-share scale-up](#fair-share-scale-up)
  - [Role-aware allocation (P/D)](#role-aware-allocation-pd)
  - [Scale-down](#greedy-scale-down)
- [Shared building blocks](#shared-building-blocks)
- [Observability](#observability)
- [Extending: writing a new optimizer](#extending-writing-a-new-optimizer)
- [References](#references)

## The ScalingOptimizer interface

Defined in `internal/engines/pipeline/optimizer_interfaces.go`:

```go
type ScalingOptimizer interface {
    // Name returns optimizer identifier for logging/metrics.
    Name() string

    // Optimize produces VariantDecisions from analyzer results and optional constraints.
    // constraints may be nil in unlimited mode.
    Optimize(ctx context.Context, requests []ModelScalingRequest, constraints []*ResourceConstraints) []interfaces.VariantDecision
}
```

The optimizer receives **one `ModelScalingRequest` per model** and produces a flat
slice of `VariantDecision` values across all models:

```go
type ModelScalingRequest struct {
    ModelID       string
    Namespace     string
    Result        *interfaces.AnalyzerResult   // capacity signals from the analyzer
    VariantStates []interfaces.VariantReplicaState
    Priority      float64 // model priority (default 1.0)
    Disaggregated bool    // true when model has prefill+decode variants
}
```

## Inputs and outputs

The optimizer consumes the analyzer's `AnalyzerResult`
(`internal/interfaces/analyzer.go`). The fields that matter to optimizers:

| Field | Meaning |
|-------|---------|
| `RequiredCapacity` | `> 0` ⇒ scale-up needed (in analyzer units, e.g. tokens) |
| `SpareCapacity` | `> 0` ⇒ scale-down possible |
| `Score` | composite priority score: `priority * Σ(requiredCapacity_i · analyzerScore_i)`; used by greedy fair-sharing |
| `VariantCapacities` | per-variant `Cost`, `PerReplicaCapacity`, `AcceleratorName`, `Role`, `Utilization` |
| `RoleCapacities` | per-role (`prefill`/`decode`) capacity aggregation for disaggregated models; `nil` otherwise |

Each variant carries `Cost` and `PerReplicaCapacity`. Two derived quantities drive
the algorithms:

- **Cost-efficiency** = `Cost / PerReplicaCapacity` — cost per unit of capacity.
  Lower is better; this is what scale-up prefers.
- **Absolute cost** = `Cost` — the per-replica price. Higher is what scale-down
  removes first.

The output is a `VariantDecision` per variant with `CurrentReplicas`,
`TargetReplicas`, an `Action` (`ScaleUp` / `ScaleDown` / `NoChange`), and a
human-readable `Reason` that names the active optimizer for observability.

## Optimizer selection

Selection happens in the engine (`internal/engines/saturation/engine.go`) on every
reconcile, driven by the `enableLimiter` flag in the saturation scaling config:

```go
if enableLimiter {
    e.optimizer = pipeline.NewGreedyByScoreOptimizer() // limited mode
} else {
    e.optimizer = pipeline.NewCostAwareOptimizer()     // unlimited mode (default)
}
```

Because both optimizers are stateless, swapping them between reconciles is safe.
See [Saturation Scaling Configuration](saturation-scaling-config.md) for how to set
`enableLimiter`.

## CostAwareOptimizer (unlimited mode)

`internal/engines/pipeline/cost_aware_optimizer.go`

The default optimizer. It assumes GPU capacity is **unconstrained** and processes
each model **independently** — there is no competition between models. It ignores
the `constraints` argument entirely. The goal is to satisfy each model's capacity
requirement at the lowest possible cost.

### <a name="cost-aware-scale-up"></a>Scale-up

When `RequiredCapacity > 0`:

1. Sort variants by **cost-efficiency ascending** (cheapest capacity first).
2. Walk the sorted list, adding replicas to each variant until the required
   capacity is met. Replicas needed per variant = `ceil(remaining / PerReplicaCapacity)`.
3. Respect each variant's `maxReplicas`. When a variant hits its cap, the
   remaining capacity **spills over** to the next-most-efficient variant.

This concentrates load on the most cost-efficient variant and only spreads to
pricier variants when the cheap one is capped.

### <a name="cost-aware-scale-down"></a>Scale-down

When `SpareCapacity > 0`:

1. Sort variants by **absolute cost descending** (most expensive first).
2. Remove replicas from the most expensive variant first:
   `floor(remaining / PerReplicaCapacity)`, bounded by removable headroom.
3. Respect each variant's `minReplicas` annotation floor.
4. The **cheapest variant is protected at ≥1 replica** — but *only* when no other
   variant still has replicas. This prevents scale-down deadlocks where the
   expensive variant's per-replica capacity exceeds spare yet cheaper replicas
   could safely be removed.

## GreedyByScoreOptimizer (limited mode)

`internal/engines/pipeline/greedy_score_optimizer.go`

Used when GPUs are scarce and models compete for them (`enableLimiter: true`). It
respects hard resource constraints and **fair-shares** GPUs across models rather
than satisfying each in isolation.

Key differences from `CostAwareOptimizer`:

- Respects `ResourceConstraints` (per-accelerator-type GPU budgets).
- Fair-shares GPUs across competing models, ordered by composite `Score`.
- For disaggregated models, distributes replicas between prefill/decode roles
  proportional to per-role demand.
- **Scale-down reuses `CostAwareOptimizer`'s logic** — fairness only matters when
  adding replicas under scarcity.

### Resource constraints

In limited mode the engine computes a `ResourceConstraints` from the GPU limiter
before calling the optimizer:

```go
type ResourcePool struct {
    Limit int // total capacity (from cluster discovery)
    Used  int // currently in use
}
// Available() = max(0, Limit - Used)

type ResourceConstraints struct {
    ProviderName string                  // e.g. "gpu-limiter"
    Pools        map[string]ResourcePool // accelerator type → pool
    // ... plus aggregate totals: TotalLimit, TotalUsed, TotalAvail
}
```

Multiple providers can contribute constraints; `mergeConstraints` takes the
**most restrictive** availability per accelerator type.

A constraint is attached **only** when the GPU limiter is a `DefaultLimiter` *and*
constraint computation succeeds. If computation fails (or the limiter is a
different type), no constraint is attached and the greedy optimizer receives an
**empty GPU budget** — `fairShareScaleUp` then sees zero available GPUs and
performs **no scale-up that cycle** (scale-down is unaffected). Note this is *not*
the same as cost-aware/unlimited behavior, despite the engine's
`"falling back to unlimited"` log message.

### Fair-share scale-up

Scale-up uses **iterative mean-based fair-sharing** so no single model starves the
others:

1. Split requests into scale-up work (`RequiredCapacity > 0` or `Score > 0`) and
   everything else (scale-down / steady).
2. Each round over the active scale-up models:
   - Stop if no GPUs remain in any pool.
   - Compute the **mean remaining score** across active models.
   - Pick the **most starved** model (highest remaining score).
   - Allocate replicas to pull its remaining score down toward the mean (its full
     remaining demand if it is the only active model), drawing from the cheapest
     eligible variants whose accelerator still has GPUs.
   - Drop the model from further rounds if it received **no** replicas (no eligible
     GPUs) **or** if it is **still above the mean** after allocating — i.e. GPU or
     `maxReplicas` limits prevented it from being pulled down, so it has had its
     turn. A model pulled *to or below* the mean stays active and may receive more
     in a later round, once the mean has dropped.

Allocation honors `GPUsPerReplica`, available GPUs per accelerator type, and each
variant's `maxReplicas`. Every allocated replica decrements the corresponding
accelerator pool, so the budget is enforced continuously.

### Role-aware allocation (P/D)

For prefill/decode **disaggregated** models, `buildScaleUpWork` computes a demand
fraction per role from `RoleCapacities`:

```
roleDemand[role] = RoleCapacities[role].RequiredCapacity / Σ RequiredCapacity
```

`allocateByRole` then splits the model's allocation target across roles
proportional to those fractions, **higher-demand roles first**. If a role can't be
fully satisfied (e.g. its accelerator is exhausted) or has no matching variants,
its unallocated share is *consumed* rather than spilled into other roles — this
keeps the next iteration from over-allocating one role at another's expense.

### <a name="greedy-scale-down"></a>Scale-down

Models that are scaling down or steady bypass fair-sharing and go straight through
`costAwareScaleDown` — identical to the [cost-aware scale-down](#cost-aware-scale-down)
above.

## Shared building blocks

Both optimizers share helpers in `cost_aware_optimizer.go`:

| Helper | Purpose |
|--------|---------|
| `buildStateMap` / `buildCapacityMap` | index `VariantReplicaState` / `VariantCapacity` by variant name |
| `initTargets` | seed replica targets from current replica counts |
| `sortByCostEfficiencyAsc` | order variants cheapest-capacity-first (scale-up) |
| `sortByCostDesc` | order variants most-expensive-first (scale-down) |
| `findCheapestVariant` | identify the variant protected at ≥1 replica |
| `buildDecisionsWithOptimizer` | convert the targets map into `VariantDecision`s, stamping action/reason |
| `mergeConstraints` | combine multi-provider constraints, most-restrictive wins |

After the optimizer runs, the engine hands its decisions to the **enforcer**
(`enforcer.go`), which applies scale-to-zero or minimum-replica policy, and then
enriches decisions with KV cache token data before they are applied.

## Observability

The active optimizer is exported as a gauge:

- **`wva_optimizer_active`** — `1` for the active optimizer, `0` for inactive,
  labeled by optimizer name (`cost-aware`, `greedy-by-score`). The engine
  re-records this whenever the optimizer changes.

Per-decision allocation detail is logged at `DEBUG` verbosity (scale-up/scale-down
allocations, fair-share iterations, per-role consumption). Each `VariantDecision`
`Reason` string also names the optimizer that produced it, e.g.
`V2 scale-up (optimizer: greedy-by-score, required: 1200)`.

See [Metrics & Health Monitoring](metrics-health-monitoring.md) for the full metric
catalog.

## Extending: writing a new optimizer

To add an optimizer:

1. Implement the `ScalingOptimizer` interface in `internal/engines/pipeline/`.
2. Reuse the shared helpers (`buildStateMap`, `initTargets`,
   `buildDecisionsWithOptimizer`, …) so output shape and reason strings stay
   consistent.
3. Wire selection into the engine alongside the existing
   `NewCostAwareOptimizer` / `NewGreedyByScoreOptimizer` branch.
4. Add it to the `wva_optimizer_active` recording set so its activity is observable.
5. Cover scale-up, scale-down, `maxReplicas`/`minReplicas` bounds, and (if relevant)
   role-aware behavior with table-driven tests, following
   `cost_aware_optimizer_test.go` and `greedy_score_optimizer_test.go`.

## References

- `internal/engines/pipeline/optimizer_interfaces.go` — the `ScalingOptimizer` interface
- `internal/engines/pipeline/cost_aware_optimizer.go` — unlimited-mode optimizer + shared helpers
- `internal/engines/pipeline/greedy_score_optimizer.go` — limited-mode fair-share optimizer
- `internal/engines/pipeline/limiter_interfaces.go` — `ResourceConstraints` / `ConstraintProvider`
- `internal/engines/pipeline/enforcer.go` — post-optimizer policy enforcement
- `internal/interfaces/analyzer.go` — `AnalyzerResult`, `VariantCapacity`, `RoleCapacity`
- [Saturation Scaling Configuration](saturation-scaling-config.md) — `enableLimiter` and analyzer selection
- [Throughput Analyzer](throughput-analyzer.md) / [Queue Model Analyzer](slo-queuemodel.md) — analyzers that feed the optimizers
