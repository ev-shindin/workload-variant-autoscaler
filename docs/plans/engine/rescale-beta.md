# Plan: Priority-Weighted Rescale — Beta stage

Implements the **Beta** milestone of the rescale proposal
(issue [#1447](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1447)),
building on the **Alpha** redistribution (`docs/plans/engine/rescale-alpha.md`, PR #1452).

## Goal

Alpha made rescale correct but silent and un-damped. Beta adds the two things Alpha
explicitly deferred:

1. **Observability** — make each cycle's redistribution visible on the
   `VariantAutoscaling`, and surface when a reclaim is *blocked*.
2. **Hysteresis** — stop reclaim/fill from oscillating when demand hovers around the
   contention threshold.

Neither changes the Alpha water-fill math, the scope-coupling rules, or the paced
free-capacity-gated convergence. With the new knobs at their defaults (0), behaviour is
identical to Alpha.

## Scope

**In Beta**
- `Rescaled` status condition, per model, carrying priority, the water-fill **share**
  vs current GPUs, the budget scope, and the direction taken.
- **Reclaim-stall** surfacing: a role that owed a reclaim but could not shed a whole
  replica (blocked by `minReplicas` or the cheapest-at-1 protection) is flagged, sets
  the `Stalled` condition reason, and emits a `RescaleStalled` **Warning** event.
- **Deadband** hysteresis (`rescaleMinShareGapGPUs`) — hold a model when
  `|share − current|` is below the gap, in both directions.
- **Reclaim cool-down** (`rescaleCooldownSeconds`) — suppress a repeat reclaim from a
  model until the configured time has elapsed since its last reclaim.

**Still deferred (Stable)**
- Physical∧quota **namespace partition** of the cluster budget → needs #1003.
- **Multi-accelerator** models (variants spanning GPU types).

## Config — hysteresis knobs (budget-scope)

Two fields on `SaturationScalingConfig`, read only from the **`default`** entry, with
the same budget-scope semantics as `enableRescale` (global governs the cluster budget;
a namespace-local `default` governs that namespace's quota; never the global fallback
for a namespace). Both default `0` (off).

| Knob | Effect |
|---|---|
| `rescaleMinShareGapGPUs` | Deadband in whole GPUs. Hold the model when `0 < |share − current| ≤ gap` (inclusive, so `gap: 1` damps a single-GPU flap). |
| `rescaleCooldownSeconds` | Per-model reclaim cool-down in seconds. Suppress a repeat reclaim within the window. |

Recommended when enabling rescale in production: `gap: 1`, `cooldown: ~60`.

Resolved per scope via `config.RescaleTuningCluster` /
`config.RescaleTuningForNamespaceLocal`, mapped into `pipeline.RescaleFlags`
(`ClusterTuning`, `ByNamespaceTuning`) by `resolveRescaleFlags`.

## Hysteresis semantics

Applied per model in `rescaleModelDecisions`, on the model-level `share` vs `current`
delta (before the per-role split):

- **Deadband** holds both directions: a sub-gap reclaim *and* a sub-gap fill are
  suppressed, so tiny share drift does not actuate.
- **Cool-down** holds only a would-be **reclaim**; fills are never cooled down, so
  genuinely free GPUs still reach higher-priority work. The cool-down is stamped only
  when a reclaim actually **shed** GPUs — a *stalled* reclaim (shed nothing) never
  starts a cool-down, so the retry that could finally succeed is not blocked.
- A held model produces no reclaim/fill (targets stay at current) and reports the
  `hold` action.

State: the cool-down clock (`time.Now()` for the cycle) and an engine-owned
`map[modelKey]time.Time` last-reclaim store are handed to the optimizer each cycle. The
store is touched only by the single optimize goroutine (lock-free) and is pruned to the
live model set every cycle. Both knobs are inert at 0 — the deadband is inclusive
(`0 < |gap| ≤ knob`) but `absGap ≥ 1` while `knob == 0` never satisfies it, and the
cool-down window can never trigger — so Alpha behaviour is preserved byte-for-byte.

**Known limitations (deferred):**
- The deadband is evaluated on the *model-level* gap and, when it holds, skips the whole
  per-role loop. For a disaggregated (P/D) model whose total sits inside the band while its
  role split swings, a budget-neutral prefill↔decode rebalance is frozen for that cycle.
  This only bites at `gap ≥ 2` with a specific imbalanced-P/D shape (the recommended
  `gap: 1` never triggers it), so it is left as a future refinement (a per-role deadband).
- The condition/event message reports the model-level `share`/`current` GPUs. For a P/D
  model whose roles rebalance to a near-zero net delta, a genuine role-level stall can
  therefore surface with `share == current` in the text — accurate at the model level but
  hiding the role imbalance that caused it. Surfacing role-level numbers is a future polish.

## Observability

- `domain.RescaleDecisionInfo` (attached to every rescale decision; nil otherwise)
  carries model-level `Priority`, `TargetGPUs` (share), `CurrentGPUs`, `Scope`, plus a
  **per-variant** `Action` (`reclaim`/`fill`/`hold`) and `Stalled`. Action/Stalled are
  keyed off the variant's own role (`VariantDecision.Role`), not one shared model-level
  summary, so a P/D model whose decode role stalls while its prefill role fills does not
  blame the grown prefill variant with the decode reclaim/stall. A no-change variant
  reflects its role's intent (a fill gated to 0 this cycle still reads `fill`; a reclaim
  blocked at `minReplicas` still reads `reclaim`); a scaled-up variant is never `Stalled`.
- `reclaimRole` returns the GPUs it could not shed; a whole-replica-sized shortfall
  marks that **role** stalled, attributed to that role's variants.
- The engine's `applySaturationDecisions` calls `setRescaledCondition`, which sets the
  `Rescaled` condition (`variant.TypeRescaled`) with reason
  `Reclaim`/`Fill`/`Hold`/`Stalled` and a message
  `priority=… share=…GPU current=…GPU scope=…`. A stall additionally emits the
  `RescaleStalled` Warning event (added to `recordEvent`'s dedup-bypass list so it
  coexists with a scaling event) and a log line. When a VA is no longer rescaled (its
  group became uncontended, or rescale was disabled), a previously-`True` `Rescaled`
  condition is flipped to `False` so it does not linger as a stale "still reclaiming".

## Files

| File | Change |
|---|---|
| `internal/config/saturation_scaling.go` | `RescaleMinShareGapGPUs`, `RescaleCooldownSeconds` + `Validate` (>= 0) |
| `internal/config/config.go` | `RescaleTuning` + `RescaleTuning{Cluster,ForNamespaceLocal}` accessors (bool accessors delegate) |
| `internal/domain/saturation_analyzer.go` | `RescaleDecisionInfo` + `Rescale` field + action constants |
| `internal/variant/types.go` | `TypeRescaled` + `ReasonRescale{Reclaim,Fill,Hold,Stalled}` |
| `internal/constants/constants.go` | `K8SEventRescaleStalled` |
| `internal/engines/pipeline/rescale.go` | per-scope tuning; deadband + cool-down; stall detection; info payload |
| `internal/engines/pipeline/greedy_score_optimizer.go` | `RescaleNow` + `RescaleLastReclaim` per-cycle state |
| `internal/engines/saturation/engine.go` | last-reclaim store; wire clock/store/tuning; `setRescaledCondition`; stall event |
| `config/base/manager/saturation-scaling-configmap.yaml`, `deploy/configmap-saturation-scaling.yaml` | document the two knobs |

## Tests
- Config: knob yaml tags + zero defaults + negative-value validation; tuning accessors
  (cluster + namespace-local, no global fallback).
- Observability: info payload on reclaim/fill, namespace scope, the stall path, off ⇒
  nil; condition/reason/message + stall Warning event.
- Hysteresis: deadband holds a sub-gap reclaim; cool-down suppresses then allows across
  cycles; no cool-down stamp on a stalled reclaim; **knobs=0 ⇒ decisions identical to
  Alpha even with the clock wired**.

## Commits (each: build + `go test` + golangci-lint via WSL, DCO-signed)
1. **Config knobs** — the two fields + accessors + configmap docs. Behavior-neutral.
2. **Observability** — `RescaleDecisionInfo` + `Rescaled` condition/reasons + stall
   detection + event, with tests.
3. **Hysteresis** — per-scope tuning + deadband + cool-down + engine state wiring, with
   tests.
4. **Docs** — this plan; alpha deferred-note update.
