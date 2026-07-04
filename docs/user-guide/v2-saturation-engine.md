# Multi-Analyzer Capacity Engine (V2)

The Workload Variant Autoscaler (WVA) is a **global optimizer**: each cycle it computes how many
replicas each inference workload should have. It does not patch replica counts directly — it
**recommends a target** that HPA or KEDA actuate.

WVA has two scaling engines. This guide covers the newer **multi-analyzer capacity engine**
(hereafter *the capacity engine*; called **V2** in code and metrics), which builds a capacity model
in absolute KV-cache tokens. It is a small pipeline: an **analyzer** produces a *capacity view* (a
supply-vs-demand computation), an **optimizer** turns that into per-variant replica targets, and an
optional **limiter** caps those targets to real resource limits. It runs a single analyzer by
default; more can be enabled (see
[Inside the engine](#inside-the-engine-analyzers-optimizer-limiter)). The older **percentage-based
engine** (**V1**) is a single fixed analyzer that reasons in per-replica utilization percentages.
This guide explains the terms the capacity engine uses, the metrics it reads, the thresholds that
govern it, and the algorithm that turns those into replica targets.

It is aimed at operators tuning and observing WVA. For the full configuration field reference and
the EPP coordination workflow, see
[Saturation Scaling Configuration](../developer-guide/saturation-scaling-config.md); for the
complete metrics catalog see
[Metrics & Health Monitoring](../developer-guide/metrics-health-monitoring.md).

## Terminology

| Term | Meaning |
|---|---|
| **Model** | A served model (a `modelID`, e.g. `Qwen/Qwen3-0.6B`) in a namespace. The unit WVA makes a scaling decision for. |
| **Variant** | One deployable configuration of a model — a Deployment/LeaderWorkerSet, typically pinned to an accelerator type (and, for disaggregated serving, a role). A model may have several variants (e.g. A100 vs H100, or prefill vs decode). |
| **Replica** | One running instance of a variant. For a Deployment that is a single pod; for a LeaderWorkerSet it is one **group** — a leader pod plus zero or more worker pods (capacity and GPUs are counted per group). |
| **Role** | For prefill/decode-**disaggregated** serving, a variant serves the `prefill` or `decode` stage; non-disaggregated variants are `both`. WVA infers the role from the variant's (leader) pod label **`llm-d.ai/role`** — `prefill` or `decode`; any other value, or no label, means `both`. |
| **EPP** | *Endpoint Picker* — the request router in the llm-d inference scheduler ([Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension)). EPP spreads requests across replicas; WVA recommends how many replicas should exist. |
| **Capacity / supply** | How much work a replica (or the whole model) can serve, measured in **KV-cache tokens**. |
| **Demand** | How much work the model currently needs to serve, also in KV-cache tokens. |
| **Cost** | A per-replica price for a variant that the `cost-aware` optimizer uses to prefer cheaper variants. Set on the `VariantAutoscaling` resource, or via the `llm-d.ai/variant-cost` annotation on the HPA/KEDA object (annotation mode); defaults to `10.0`. |
| **Score** | A non-negative weight (default `1.0`) on an analyzer's contribution, used only when **several** analyzers run — it sets how their signals combine, and (under the capacity engine's limiter) feeds the cross-model fair-share ranking. With a single analyzer every model carries the same score, so it cancels out and changes nothing. |
| **Priority** | The relative weight a model gets when the `greedy-by-score` optimizer fair-shares scarce GPUs across models (default `1.0`). Applies **only** on the capacity engine (V2) with the limiter on; ignored when the limiter is off, and ignored on V1 (whose limiter shares GPUs by saturation, not priority). |

## Overview

WVA ships two scaling engines:

| Aspect | **Percentage-based engine (V1)** | **Multi-analyzer capacity engine (V2)** |
|---|---|---|
| Reasons in | KV-cache utilization *fractions* per replica | Absolute **KV-cache tokens** (supply vs. demand) |
| Structure | a single fixed analyzer | pluggable **analyzers** → **optimizer** → optional **limiter** |
| Scale-up size | count of replicas that "look saturated" | a division: `ceil(required tokens / per-replica capacity)` |
| Multi-replica jumps | limited | native |
| Heterogeneous accelerators | not modeled | first-class (per-variant capacity) |
| Sizing a scaled-to-zero variant | 0→1 wake-up only¹ | 0→N — target from a stored estimate (measured or deployment-derived), else a compatible sibling |
| GPU limiter (`enableLimiter`) | trims decisions post-hoc to fit GPUs (most-saturated-first, no `priority`) | GPU limits constrain the optimizer's placement (`priority` + `score`) |

¹ Waking a scaled-to-zero variant to a single replica on incoming traffic is handled by a **separate
scale-from-zero engine** that runs regardless of which saturation engine is active — so V1 workloads
scale from zero too. What V2 adds is *sizing* a zero-replica variant to the right number of replicas
(0→N) rather than only 0→1.

The multi-analyzer capacity engine (V2) is the preferred engine going forward (the
percentage-based one is the legacy path), but it is **opt-in**: the shipped default
(`analyzerName: ""`) still runs V1, so you enable V2 explicitly (below). It builds an explicit
capacity model in units of KV-cache tokens, so a single decision can raise the target by several
replicas at once (e.g. 2 → 5) and can even **size a variant up from zero** (0→N) — computing a
multi-replica target for a variant that has no running pods to measure (see [step 1](#1-per-replica-capacity-calculation)).

## Enabling and operating the engine

### Enabling the engine

Enable the engine from the saturation-scaling ConfigMap's global `default` entry, either way:

```yaml
analyzerName: "saturation"     # "" selects the percentage-based engine (V1)
```

or, equivalently, with the **multi-analyzer list form** — the same field you use to add more
analyzers as they land (each with a `score` weight that sets its influence on the combined result):

```yaml
analyzers:
  - name: saturation
    score: 1.0
```

Both forms are equivalent — WVA back-fills the `analyzers:` list from `analyzerName`, and both report
`analyzer_name="saturation"` on the `wva_config_info` metric (see
[Verify the engine is active](#verify-the-engine-is-active)). Use `analyzerName: "saturation"` for
the common case; use the `analyzers:` list when you want to set a per-analyzer `score` or threshold
override, or to enable a second analyzer. The list ships with `saturation` only — an experimental
throughput analyzer exists but is off by default (see
[Inside the engine](#inside-the-engine-analyzers-optimizer-limiter)).

Engine selection is read from the `default` entry only — it is **global**. Per-model and
per-namespace entries can override *thresholds*, but not which engine runs.

### Verify the engine is active

Check `wva_config_info` — the active engine appears in the `analyzer_name` label:

```promql
wva_config_info{analyzer_name="saturation"}   # == 1 when the engine is running
```

If it shows a different `analyzer_name` (or the metric is absent), the engine is not selected — confirm
the ConfigMap's `default` entry sets `analyzerName: "saturation"`. On the capacity engine with the
limiter on, `wva_optimizer_active{optimizer_name="greedy-by-score"} == 1` confirms fair-share mode
(otherwise `optimizer_name="cost-aware"`) — the optimizers are described in
[Inside the engine](#inside-the-engine-analyzers-optimizer-limiter). V1 has no optimizer, so there is
no `greedy-by-score` series there; to confirm the limiter is active on either engine, watch
`wva_decisions_limited_total`.

### Why isn't it scaling?

Work down this list:

1. **No inputs.** `wva_metrics_pods_discovered == 0`, or `wva_metrics_freshness_status{status="stale"|"missing"}` — WVA isn't getting fresh metrics. Check pod discovery / scraping.
2. **Collection errors.** `wva_metrics_collection_errors_total` climbing — a query is failing (labeled by `query_type`).
3. **Not saturated.** `wva_required_capacity == 0` (roughly, utilization below `scaleUpThreshold`) — expected no-change.
4. **Scale-up already in flight.** Pending replicas count toward *anticipated* supply (see [step 3](#3-the-scaling-signal)), so `wva_required_capacity` stays `0` until they become ready.
5. **Capped by GPUs.** With `enableLimiter: true` (the [limiter](#inside-the-engine-analyzers-optimizer-limiter)), `wva_available_gpus` at/near 0 (or namespace quota exhausted) blocks scale-up even when demand warrants it. GPU discovery runs **only while the limiter is enabled**, so if you turn it on and scale-up freezes, check `wva_available_gpus`: an **absent or zero** series means WVA discovered no accelerators (wrong node labels, no GPU operator) and is capping every model to `0` additional replicas. `rate(wva_decisions_limited_total[5m]) > 0` confirms the limiter is actively cutting targets (cap-by-saturation on V1, fair-share on the capacity engine).
6. **Actuation.** `wva_desired_ratio > 1` but replicas don't change → the HPA/KEDA object consuming the signal isn't acting; check the actuator, not WVA.
7. **Zero-replica variant only wakes 0→1, never 0→N.** V2 couldn't derive a per-replica capacity for it (see [step 1](#1-per-replica-capacity-calculation)): WVA couldn't resolve the variant's Deployment/LeaderWorkerSet to derive a stored estimate, and there's no compatible sibling. First confirm the variant's scale target (Deployment/LWS) exists and is discoverable; failing that, that a sibling with the same `modelID`, accelerator type, GPU count, and compatible engine config exists.

## Inside the engine: analyzers, optimizer, limiter

The multi-analyzer capacity engine is a small pipeline. Each cycle, per model, it runs three
stages:

**1. Analyzers** — each analyzer builds a *capacity view* (a supply-vs-demand computation in
KV-cache tokens) and carries a per-analyzer `score`.
- **By default** the engine runs a single analyzer, the **token-based saturation analyzer**
  (`analyzerName: "saturation"`) — its algorithm is the [How it works](#how-it-works) section below.
- **The engine can run several analyzers**, declared under the `analyzers:` list in the ConfigMap.
  Each entry has a `name`, an `enabled` flag (default `true` — so you can turn an analyzer off
  without removing its entry), a `score` weight, and optional per-analyzer `scaleUpThreshold` /
  `scaleDownBoundary` overrides. A **throughput analyzer** exists but is **experimental** — it is
  **off by default and not recommended for use yet** — and is not covered by this guide. When more
  than one analyzer runs, the engine combines their signals — `score` weights each analyzer's
  contribution to the fair-share ranking (see the optimizer below).

**2. Optimizer** — turns the analyzers' required/spare capacity into concrete per-variant replica
targets, deciding *where* to place replicas across a model's variants. WVA picks one of two
algorithms automatically:
- **`cost-aware`** *(default — used when the limiter is off).* Minimizes total cost while meeting
  the required capacity: it places replicas on the variant with the best capacity-per-cost (cheapest
  per token) and, on scale-down, sheds the highest-cost variant first. Scale-up is unconstrained
  — it assumes GPUs are available.
- **`greedy-by-score`** *(used when the limiter is on).* Built for GPU scarcity. Each cycle it ranks
  the competing models by a **fair-share weight** and hands the next available GPU to the
  highest-ranked (most-starved, highest-priority) model, repeating until the GPU budget is exhausted
  — so no single model monopolizes the GPUs. The fair-share weight combines a model's `priority`,
  its analyzers' `score`, and its remaining unmet need (the code name reflects the `score` factor).

**3. Limiter** *(optional)* — keeps scaling within the cluster's real GPU budget when
`enableLimiter: true`. It discovers the cluster's accelerators automatically (no quota object
required). The **key difference** is *when* GPU availability enters the decision:
- **Capacity engine (V2):** GPU availability is fed to the **optimizer** as a *constraint before it
  decides*, so the optimizer chooses which variant to scale with the GPU budget already in hand —
  fair-sharing scarce GPUs across models by `priority` (and `score`) via `greedy-by-score`. GPU
  limits shape the placement itself.
- **Percentage-based engine (V1):** the analyzer picks per-variant targets *first*, then the limiter
  **post-processes** those decisions — trimming them to fit the budget, granting to the
  **most-saturated** variants first. It only *cuts* existing decisions; it does **not** re-route a
  scale-up to a different variant, and it does **not** honor `priority`. (Same `enableLimiter` flag —
  V1 has no optimizer to fold GPU limits into the decision.) Still worth enabling on V1 as a
  **safety cap** against over-provisioning; for `priority`-weighted placement, use the capacity engine.
- **Pluggable:** a limiter pairs a resource **inventory** (the discovered GPUs) with an **allocation
  policy**, so **more limiters are planned** — e.g. operator-declared **quota caps** per accelerator
  type or namespace.

Whichever engine, `wva_decisions_limited_total` increments whenever the limiter cut a target.
Everything else in this section (the optimizer, per-analyzer scoring, `priority` fair-share) is
capacity-engine-only.

## How it works

The rest of this section describes the **token-based saturation analyzer** — the one analyzer the engine
runs today. Each cycle, for every model, it produces the capacity view in four core steps (1–4);
the engine's optimizer and optional limiter (step 5) then place and cap the replicas.

### 1. Per-replica capacity calculation

A replica can't serve past whichever wall it hits first — **memory** or **compute** — so **one
replica's** capacity is the **smaller** of two ceilings, both in KV-cache tokens:

- **Memory ceiling** = `KV_token_capacity × kvCacheThreshold`.
  `KV_token_capacity` (= `num_gpu_blocks × block_size`) is the pod's physical KV-cache size,
  read from `vllm:cache_config_info` (or derived from the deployment's engine args when the
  metric is absent). `kvCacheThreshold` (default **0.80**) reserves headroom — only 80% of the
  cache is treated as usable.
- **Compute ceiling** = how many concurrent tokens the GPU can actually *process* before
  scheduling throughput — not memory — becomes the bottleneck (common for long-generation
  workloads). The engine estimates it from live queue behavior when a replica is queue-saturated,
  otherwise from recent history, otherwise from the deployment's batch/sequence limits. *(The
  detailed estimation chain is in the [developer guide](../developer-guide/saturation-scaling-config.md).)*

The two ceilings above give **one replica's** capacity. A variant usually runs several replicas,
so the engine needs a single representative figure for it: the variant's **per-replica capacity** is the
**median** across its *ready* replicas' capacities. *Ready* here means a replica that is running
and past start-up — i.e. current replicas minus any still-pending (booting) ones (WVA reads the
workload's ready-replica status; for a LeaderWorkerSet that is the count of **fully-ready groups**,
where the leader and all its workers are ready). The median keeps one outlier pod from skewing the
value.

When a variant has **no ready replicas to measure** (for example, scaling up from zero), WVA sizes it
from a **stored capacity estimate** — a per-variant value it keeps, either previously measured from
live pods or derived from the variant's deployment/engine args (vLLM KV-cache config, `max-model-len`,
GPU count). If the variant has no stored estimate of its own, WVA borrows one from a **compatible
sibling** — another variant serving the same `modelID` on the same accelerator type and GPU count
with compatible engine config, in **any namespace** (capacity is a property of hardware + engine
config, not namespace). That per-replica capacity is what lets the engine size a variant that runs
zero replicas. If neither exists, per-replica capacity is `0` and V2 can't size the variant — the
separate scale-from-zero engine still handles the plain 0→1 wake-up (see [Overview](#overview)).

### 2. Demand calculation — the tokens the model needs to serve

The total tokens the model needs to serve is the sum of three sources, all in KV-cache tokens:

```
demand = tokens_in_use + local_queue_tokens + scheduler_queue_tokens
```

where:

- **`tokens_in_use`** — tokens for the requests **currently running** on the model's replicas
  (`kv_cache_usage × capacity`).
- **`local_queue_tokens`** — tokens for the requests **queued on the replicas** themselves
  (`num_requests_waiting × avg input tokens`).
- **`scheduler_queue_tokens`** — tokens for the requests **still queued upstream** in the EPP
  flow-control layer, before they reach any pod. Its input-token portion is discounted by the
  observed prefix-cache hit rate when available.

### 3. The scaling signal

The engine aggregates to model level:

- **`TotalSupply`** = Σ (ready replicas × per-replica capacity)
- **`TotalAnticipatedSupply`** = Σ ((ready + **pending**) replicas × per-replica capacity)
- **`TotalDemand`** = the demand above

We want demand to sit at a target fraction of supply — the **`scaleUpThreshold`** (default `0.85`)
— so the supply we *need* is `demand ÷ scaleUpThreshold` (at 0.85, demand should be 85% of supply).
The matching floor for safe scale-down is **`scaleDownBoundary`** (default `0.70`). Both are set in
the `wva-saturation-scaling-config` ConfigMap (full reference under [Thresholds](#thresholds)).
That gives two signals:

```
RequiredCapacity  = max(0, TotalDemand / scaleUpThreshold  − TotalAnticipatedSupply)   → scale up
SpareCapacity     = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)             → scale down
```

- **`RequiredCapacity > 0`** → the supply we need exceeds what's already in flight → **scale up**.
  *Anticipated* supply counts **pending** (still-booting) replicas, so an in-flight scale-up shrinks
  `RequiredCapacity` — the target won't keep growing while a slow-loading GPU replica boots, avoiding
  the overshoot a plain metric-threshold loop can hit during long cold starts.
- **`SpareCapacity > 0`** → even after inflating demand to the `scaleDownBoundary`, supply is
  left over → **scale-down is safe**.
- Neither → **no change**. Two thresholds rather than one is deliberate: a single threshold would
  flap (cross it → scale up → utilization dips below → scale down → repeat). The gap between
  `scaleUpThreshold` (0.85) and `scaleDownBoundary` (0.70) is a hysteresis **dead-band** — scale-up
  needs utilization above 0.85, scale-down below 0.70, in between holds steady. (WVA has no
  time-based stabilization window of its own yet; for time-based smoothing, set a
  `stabilizationWindowSeconds` on the consuming HPA/KEDA object. Within WVA, a wider dead-band is the
  main lever against flapping.)

> **Pending replicas that never schedule.** A pod stuck `Pending` (e.g. no free GPU) stays in
> `TotalAnticipatedSupply` with no timeout, holding `RequiredCapacity` at `0` and freezing scale-up
> until it becomes ready or is removed — symptom: `wva_required_capacity == 0` with `Pending` pods on
> the variant. `enableLimiter: true` avoids creating such replicas; see
> [Why isn't it scaling?](#why-isnt-it-scaling) item 5.

> The engine also reports a model `utilization = TotalDemand / TotalSupply` for observability, but the
> decision is driven by `RequiredCapacity`/`SpareCapacity` above — **not** by comparing that
> ratio to the thresholds. (This ratio is exported as `wva_saturation_utilization`; see
> [Metrics](#metrics).)

### 4. Replica target

```
scale-up replicas   = ceil(RequiredCapacity / per_replica_capacity)
scale-down replicas = floor(SpareCapacity   / per_replica_capacity)
```

`RequiredCapacity` is in KV-cache **tokens**, and any positive value rounds *up* to at least one
replica — so in principle a 1-token deficit adds a replica. That's intended: the buffer is the
`scaleUpThreshold` itself, not a token dead-band. `RequiredCapacity` only goes positive once demand
exceeds `scaleUpThreshold × anticipated supply` (85% by default), so a small positive value means
demand has *already* crossed that target; from there `ceil(...)` rounds up because a whole replica is
the smallest unit you can add. If you don't want to scale on a small deficit, **raise
`scaleUpThreshold`** (that alone sets scale-up eagerness) — widening the gap to `scaleDownBoundary`
only changes the dead-band against flapping, not the scale-up trigger.

Scale-down respects each variant's `minReplicas` (set on the `VariantAutoscaling` resource); when a model has **multiple variants** it removes
the highest-cost variant's replicas first.

For prefill/decode-**disaggregated** models, capacity is computed **per role**: WVA groups the
model's variants by their `llm-d.ai/role` pod label (`prefill` / `decode`), builds a separate
supply-vs-demand view for each role, and reports `wva_required_capacity` / `wva_spare_capacity`
per role. It then scales the two roles **together** — dropping decode replicas without matching
prefill (or vice-versa) would unbalance the pipeline and can leave the model non-functional, so the
engine never scales one role on its own.

### 5. Optimizer and (optional) limiter

Steps 1–4 produced the token capacity view. The **optimizer** now turns it into per-variant replica
targets, and — with `enableLimiter: true` — the **GPU limiter** caps those targets to available
accelerators. Which optimizer runs, and how GPUs are fair-shared under the limiter, is covered in
[Inside the engine](#inside-the-engine-analyzers-optimizer-limiter) (the one that ran is visible in
`wva_optimizer_active`). Both optimizers share the same scale-down math
(`floor(SpareCapacity / per-replica capacity)`) and differ only in scale-*up* placement.

## Why the multi-analyzer capacity engine — advantages over the percentage-based engine (V1)

The percentage-based engine (V1) reasons about each replica's *current* KV-cache utilization as a
fraction and scales by counting how many replicas "look saturated." The capacity engine's absolute token model
unlocks scaling behaviors V1 cannot express:

- **Right-sized steps, not one replica at a time.** V1 nudges the count by the number of saturated
  replicas; the capacity engine computes the exact token deficit and sets a target
  `ceil(RequiredCapacity / per-replica capacity)` replicas higher — so it can jump 2 → 5 in a single
  cycle instead of creeping up one replica per cycle (and, on scale-down, drops it by
  `floor(SpareCapacity / per-replica capacity)` at once).
- **Compares and sizes heterogeneous accelerators.** Because capacity is an absolute token count
  per variant, the capacity engine can weigh an A100 variant against an H100 variant and place replicas where they
  are most cost-effective. V1's per-replica *percentages* are not comparable across replicas of
  different capacity.
- **Sizes up from zero, not just 0→1.** The capacity engine derives a variant's per-replica capacity
  from a stored estimate (or a compatible sibling) when there are no running pods to measure,
  so it can compute a full multi-replica target (0→N) for a variant that currently has zero replicas.
  (Waking a scaled-to-zero variant to a single replica is handled for *both* engines by the separate
  scale-from-zero engine; V1 just can't size beyond that first replica.)
- **Anticipates queued load instead of only reacting.** The capacity engine's demand includes requests still queued
  locally and upstream in the EPP flow-control layer — load that has not reached a pod yet — so it
  scales up *before* a backlog turns into latency. V1 only sees the KV-cache fraction already on the
  pods.
- **Models the compute ceiling, not just memory.** The capacity engine caps per-replica capacity at the smaller of
  the memory and compute ceilings, so it will not over-provision KV memory for a workload that is
  actually compute-bound (e.g. long generations).
- **Avoids double-scaling and oscillation.** The capacity engine counts pending (still-booting) replicas as
  *anticipated* supply, so an in-flight scale-up suppresses further scale-up until it lands; and the
  band between `scaleUpThreshold` and `scaleDownBoundary` is a stable dead-band.
- **Places replicas across variants, and fair-shares by priority.** The capacity engine runs an **optimizer**
  that decides *which* variant scales — cheapest-first (`cost-aware`), or, with the limiter on, GPU
  fair-share across competing models by `priority` and `score` (`greedy-by-score`). V1 has no
  optimizer: it counts saturated replicas with no cross-variant placement (its limiter can only trim,
  not re-route — see [Inside the engine](#inside-the-engine-analyzers-optimizer-limiter)).
- **Handles P/D disaggregation.** For prefill/decode-disaggregated models the capacity engine sizes and couples the
  two roles so the model stays balanced; V1 has no role model.
- **Extensible.** Because it is a pipeline (analyzers → optimizer → limiter), new capabilities —
  additional analyzers (throughput/SLO) and additional limiters (quota caps) — plug into the same
  engine as they ship, rather than needing a new one. The percentage-based engine is a single fixed
  analyzer.
- **Explains its decisions in the same units it uses.** The capacity engine emits token-level signals
  (`wva_required_capacity`, `wva_spare_capacity`) plus a utilization ratio (`wva_saturation_utilization`),
  so an operator can see *why* it scaled.

## Thresholds

Set these in the saturation-scaling ConfigMap (see the
[configuration reference](../developer-guide/saturation-scaling-config.md) for structure and
per-model/per-namespace overrides).

| Key | Default | Range | Effect |
|---|---|---|---|
| `kvCacheThreshold` | `0.80` | 0–1 | Fraction of physical KV cache treated as usable → sets the **memory ceiling**. Lower = more headroom. **Agree with EPP — [see below](#aligning-thresholds-with-epp).** |
| `queueLengthThreshold` | `5` | ≥ 0 | Local waiting-queue depth at/above which a replica is "queue-saturated," letting its current token load set the **compute ceiling**. **Agree with EPP — [see below](#aligning-thresholds-with-epp).** |
| `scaleUpThreshold` | `0.85` | (0, 1], `> scaleDownBoundary` | Target peak utilization. Drives **`RequiredCapacity`**. Lower = scale up earlier. |
| `scaleDownBoundary` | `0.70` | (0, 1], `< scaleUpThreshold` | Utilization floor for safe scale-down. Drives **`SpareCapacity`**. The gap to `scaleUpThreshold` is the dead-band. |
| `enableLimiter` | `false` | bool | Turns on the GPU limiter (step 5) — fair-shares GPUs across models on the capacity engine, cap-only on V1. WVA discovers the cluster's accelerators automatically — no quota object required; confirm `wva_available_gpus` reports series once enabled. |
| `priority` | `1.0` | ≥ 0 | Relative weight when the capacity engine (V2) fair-shares scarce GPUs (V2 + limiter only; inert on V1, which shares by saturation). |
| `analyzerName` / `analyzers` | `""` / — | — | Selects the engine (`"saturation"`) and, via the `analyzers` list, per-analyzer scores and threshold overrides. |

> **Not used by the capacity engine:** `kvSpareTrigger` and `queueSpareTrigger` are **V1-only**
> signals. The capacity engine still accepts and exports them as config metrics, but does not
> consult them in its decision.

**Tuning guidance** — each threshold is a knob; here's when to turn it:
- **`scaleUpThreshold`** — the target peak utilization. *Lower it* (e.g. 0.75) for latency-sensitive
  workloads or bursty traffic, so WVA scales up earlier with more headroom. *Raise it* (e.g. 0.90) to
  pack more load onto each replica — fewer replicas, lower cost — accepting less burst headroom.
- **`scaleDownBoundary`** — the utilization floor for reclaiming replicas. *Raise it* (closer to
  `scaleUpThreshold`) to shed idle replicas sooner and save GPUs; *lower it* to hold replicas longer
  for stability. The **gap** between the two is the dead-band: *narrow it* if scaling feels too
  sluggish, *widen it* if you observe flapping (repeated up/down every few cycles). Note the tension:
  raising it toward `scaleUpThreshold` reclaims GPUs faster but also narrows the dead-band.
- **`priority`** — *raise it* on a model that should win GPUs under contention; leave at `1.0` for
  equal fair-share. Takes effect only on the capacity engine (V2) with the limiter on — inert on V1
  and when the limiter is off.
- **`enableLimiter`** — *turn it on* whenever GPUs are contended, or as a hard safety cap against
  recommending more replicas than there are free GPUs; also required for `priority` to have any
  effect.

The next two are **coordination** knobs, not free-tuning ones — move them only in lockstep with EPP
(see [Aligning thresholds with EPP](#aligning-thresholds-with-epp)):
- **`kvCacheThreshold`** — *lower it* if you see KV-cache pressure or OOM risk near the limit (more
  headroom), *raise it* to use more of the cache.
- **`queueLengthThreshold`** — *lower it* to treat a replica as queue-saturated at a shorter local
  queue (react to queueing sooner); *raise it* to tolerate deeper queues before it counts.

- Set values globally (`default`) and override per model (`{modelID}#{namespace}`) — e.g. a higher
  `scaleUpThreshold` for a cost-sensitive model, a lower one for a latency-critical one.

### Aligning thresholds with EPP

`kvCacheThreshold` and `queueLengthThreshold` define **what "a saturated endpoint" means**, and
they should be **agreed with EPP** (the request router). EPP and WVA read the **same two
model-server metrics** — `vllm:kv_cache_usage_perc` and `vllm:num_requests_waiting` — but act at
different layers: EPP routes and, under load, queues/sheds requests as endpoints approach
saturation; WVA decides when the pool is saturated and recommends adding replicas. EPP's Saturation
Detector exposes two thresholds that map one-to-one to WVA's:

| EPP (`saturationDetector`) | Default | WVA | Default |
|---|---|---|---|
| `kvCacheUtilThreshold` | `0.80` | `kvCacheThreshold` | `0.80` |
| `queueDepthThreshold` | `5` | `queueLengthThreshold` | `5` |

If the two disagree, routing and scaling fight each other:
- **WVA higher than EPP** → EPP queues requests in flow-control (raising latency, and inflating
  WVA's own `scheduler_queue` demand term) while WVA still sees headroom → scale-up lags.
- **WVA lower than EPP** → WVA scales up while EPP is still comfortably balancing → over-provisioning.

**Guidance:** set WVA's `kvCacheThreshold` = EPP's `kvCacheUtilThreshold` and `queueLengthThreshold`
= EPP's `queueDepthThreshold`, and mirror any change on both sides. This repo configures EPP's
scorers (`queue-scorer`, `kv-cache-utilization-scorer`) in `deploy/lib/epp-flow-control.values.yaml`.
See the developer guide's
[Coordinating with InferenceScheduler (EPP)](../developer-guide/saturation-scaling-config.md#best-practices-coordinating-with-inferencescheduler-end-point-picker)
for the full apply/verify workflow.

**References**
- EPP EndpointPicker configuration (`saturationDetector` fields, `headroom`, `metricsStalenessThreshold`): <https://gateway-api-inference-extension.sigs.k8s.io/guides/epp-configuration/config-text/>
- EPP model-server metrics protocol (queue size, KV-cache utilization): <https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/docs/proposals/003-model-server-protocol/README.md>

## Metrics

### Input — what the engine reads from your inference engines

Scraped from vLLM and the EPP scheduler. If these are missing or stale, the engine degrades or skips the
model — check the health metrics below.

| Metric | Type | Used for |
|---|---|---|
| `vllm:cache_config_info` (`num_gpu_blocks`, `block_size`) | info gauge | KV token capacity → **memory ceiling** |
| `vllm:kv_cache_usage_perc` | gauge | live tokens-in-use → **demand** |
| `vllm:num_requests_waiting` | gauge | local queue → **compute-ceiling** trigger + demand |
| `vllm:request_prompt_tokens_sum` / `_count` | histogram | average **input** tokens per request |
| `vllm:request_generation_tokens_sum` / `_count` | histogram | average **output** tokens per request |
| `vllm:prefix_cache_hits` / `vllm:prefix_cache_queries` | counters | prefix-cache hit rate (discounts input-token demand) — *optional* |
| `inference_extension_flow_control_queue_size` / `_bytes` | gauges (EPP) | upstream scheduler-queue demand — *optional* |

Deployment engine args (`--max-num-seqs`, `--gpu-memory-utilization`, `--block-size`,
`--max-model-len`, `--max-num-batched-tokens`, `--num-gpu-blocks-override`, …) are read from the
Deployment/LeaderWorkerSet — **not** Prometheus — and feed the derived-capacity path.

### Output — WVA metrics to observe the engine's decisions

| Metric | Type | Meaning |
|---|---|---|
| `wva_saturation_utilization` | gauge | Utilization = demand / supply; `1.0` = demand equals usable capacity, **`> 1` = demand exceeds supply (saturated)**. |
| `wva_required_capacity` | gauge | > 0 ⇒ scale-up needed; value is the KV-cache **token** deficit (per-role for P/D-disaggregated models). |
| `wva_spare_capacity` | gauge | > 0 ⇒ safe scale-down headroom; value is the KV-cache **token** surplus (per-role for P/D-disaggregated models) — the companion to `wva_required_capacity`. |
| `wva_kv_cache_tokens_used` / `_capacity` | gauges | Total KV tokens in use vs. total capacity across replicas. |
| `wva_desired_replicas` / `wva_current_replicas` | gauges | What the engine wants vs. what's running. |
| `wva_desired_ratio` | gauge | desired / current — the value HPA/KEDA actuate on. |
| `wva_replica_scaling_total` | counter | Scale actions, labeled by `direction` and `reason`. |
| `wva_config_info` | gauge | Active `analyzer_name` + feature flags — confirm the engine is selected. |
| `wva_optimizer_active` | gauge | Which optimizer ran (`optimizer_name` = `cost-aware` or `greedy-by-score`). |
| `wva_config_optimization_interval_seconds` | gauge | Decision cadence. |
| `wva_models_processed` | gauge | Models handled last cycle. |
| `wva_available_gpus` | gauge | GPUs available (limiter input). |
| `wva_decisions_limited_total` | counter | Times the limiter cut a target (by `variant_name`/`namespace`/`limiter_name`) — the signal the GPU budget bit. Fires on both engines. |
| `wva_metrics_freshness_status`, `wva_metrics_pods_discovered`, `wva_metrics_collection_errors_total` | gauges / counter | **Input health** — check these first when scaling looks stuck. |

> Both `wva_required_capacity` and `wva_spare_capacity` carry a `unit` label — `continuous` on the
> capacity engine, marking a token-valued gauge (a real number of KV-cache tokens, not a replica
> count). The percentage-based engine (V1) uses `binary` / a 0–1 fraction instead.

## Example configuration

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: workload-variant-autoscaler-system   # WVA controller namespace
data:
  default: |
    analyzerName: "saturation"   # enable the engine (or the analyzers: list form — see "Enabling the engine")
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    enableLimiter: false
    priority: 1.0
  # optional per-model override:
  "my-model#my-namespace": |
    scaleUpThreshold: 0.90       # let this model run at higher utilization before scaling
```

## See also

- [Saturation Scaling Configuration](../developer-guide/saturation-scaling-config.md) — full field reference, per-model/namespace resolution, and the EPP coordination workflow.
- [Metrics & Health Monitoring](../developer-guide/metrics-health-monitoring.md) — complete metrics catalog and health endpoints.
- [Monitoring](monitoring.md) — dashboards and observing WVA.
