# Rate-anchored compute capacity (k2) — plan

## Problem

The V2 saturation analyzer models a replica's capacity as `min(k1, k2)` in KV tokens:

- `k1 = TotalKvCapacityTokens × kvCacheThreshold` — the memory bound.
- `k2` — the compute bound, currently recorded as `tokensInUse` at the moment the
  replica was seen queueing (`computeK2` priority 1), then averaged into a rolling
  history keyed by `model|accelerator|gpuCount|outputBucket`.

`k2` measures a **KV stock** while the constraint it stands for is a **rate**. On a
prefill-heavy workload the two are unrelated: the engine exhausts prompt-token
throughput and begins queueing while KV occupancy is still low. In the sustained
1000/250 comparison run the WVA leg was queueing, dropping requests, and cycling
replicas at **16.2% average KV utilization** — a regime the current model cannot
express, because at 16% occupancy `demand/supply` reads as abundant headroom.

Three consequences follow, all observed in that run:

1. **The compute bound is silently discarded.** `tokensInUse` derives from
   `kv_cache_usage` which is `max_over_time(...[1m])` — a peak. Whenever the peak
   exceeds `kvCacheThreshold` (0.80), `k2 ≥ k1` and `min(k1, k2)` returns the
   memory bound, so the compute signal never binds.
2. **Supply is over-stated exactly when load is highest**, so utilization reads low
   and `spareCapacity = totalSupply − totalDemand/scaleDownBoundary` grows.
3. **Demand deflates faster than supply.** The queue term
   (`QueueLength × (avgInput + avgOutput)`) is the dominant part of demand at peak
   and collapses to zero the moment new replicas absorb the backlog, while `k2`
   stays inflated via the historical average. The controller then sheds to one
   replica and the cycle repeats.

Tuning thresholds cannot fix this: six threshold/window/policy legs were run
against that workload and none of them broke the cycling.

## Approach

Add a second, **rate-anchored** estimator for `k2`, selected by an internal switch,
leaving the existing estimator in place and untouched when it is off.

The design separates two questions that the occupancy-based estimator conflates:

```
detector:    rates decide WHEN a replica is at its limit
measurement: tokens record WHAT that limit is
```

A replica is at its limit when it has a backlog at least `QueueLengthThreshold`
deep, or when its arrival rate has reached the service rate measured while it *was*
backlogged. At that moment its resident token count is a measurement of the limit.
That measurement is stored **per workload bucket** — model, accelerator, role,
request shape — as a running minimum, and every replica of the bucket reads the
same value.

Two properties follow, and both were learned the hard way from earlier versions of
this code:

- **The value is identical across replicas of a variant.** `aggregateByVariant`
  takes the MEDIAN of per-replica capacities. A number that varied with each
  replica's own load was not commensurable across siblings: an idle replica's
  figure blended with a backlogged one's and lifted variant capacity enough to turn
  a scale-up into a scale-down, reintroducing shed-to-one by a new route. A bucket
  ceiling makes the median a no-op.
- **The value does not move with this cycle's load.** A capacity recomputed from
  the current arrival rate changed every cycle, which is an oscillation waiting to
  happen. A stored ceiling changes only as the running minimum is lowered by a new
  measurement or relaxed upward by age — both slow by construction.

Why a limit measured at 16% KV utilization is the whole point: on a prefill-heavy
workload the engine exhausts prompt-token throughput long before memory, so the
binding constraint is invisible to an occupancy-only model. The detector notices it
from rates; the measurement records it in the units the rest of the engine speaks.

### Damping, by construction

- `SaturationEnterRatio` (0.95) means the detector fires just before arrivals cross
  the service rate, and a lambda hovering at mu does not toggle it between cycles.
- The service rate is a running maximum with slow decay; the ceiling is a running
  minimum with slow upward relaxation. Neither tracks a single cycle.
- `MinServiceRateSamples` keeps one slow interval from establishing a limit.
- lambda is smoothed over a residence time, so a completion-derived rate and an
  arrival rate are compared on the same time base.

### Why not an ITL model

`ITL(k) = A·k + B` (already fitted by the throughput analyzer) describes decode
latency against KV occupancy. On prefill-heavy traffic ITL can stay flat while TTFT
and the queue explode, so it does not capture this bottleneck. It remains the better
model for decode-bound workloads; the rate anchor covers both.

## Metrics

Everything required already exists. Two of the three are registered only when the
throughput analyzer is enabled, which this plan removes as a dependency.

| Quantity | Query | Field | Today |
|---|---|---|---|
| λ arrival rate | `rate(inference_extension_scheduler_attempts_total{status="success"}[1m])` (EPP) | `ArrivalRate` | unconditional |
| μ completion rate | `rate(vllm:request_generation_tokens_count[1m])` | `RequestRate` | throughput-analyzer only |
| occupancy | `max_over_time(vllm:kv_cache_usage_perc[1m])` × capacity | `TokensInUse` | unconditional |
| queue | `max_over_time(vllm:num_requests_waiting[1m])` | `QueueLength` | unconditional |
| KV capacity | `vllm:cache_config_info` | `TotalKvCapacityTokens` | unconditional |

No new PromQL. `QueryRequestRate` moves to a shared registrar called unconditionally
by the saturation engine (`QueryKvUsageInstant` moves with it, since the throughput
analyzer needs it and the registrar is shared); the throughput analyzer registers
them only if absent, so either order works and neither panics.

SGLang equivalents (`sglang:generation_tokens_histogram_count`, `sglang:token_usage`)
are already defined and move with them.

## Work items

1. **Shared query registration** — `internal/collector/registration/rate_capacity.go`:
   single definition of the two shared templates (vLLM + SGLang) and
   `RegisterRateCapacityQueries`; `registerIfAbsent` / `registerForEngineIfAbsent`
   helpers; `RegisterThroughputAnalyzerQueries` switched to the if-absent variants.
   Called from `saturation.Engine` construction alongside `RegisterSaturationQueries`.
2. **Switch** — `EnableRateAnchoredK2`, a build-time constant in
   `saturation_v2/rate_capacity.go`. Deliberately not a ConfigMap setting: the
   estimator is under evaluation against the incumbent and is not something to
   toggle in a running cluster. With it false the service-rate store is never
   allocated and every path in the file returns immediately.
3. **Estimator** — `internal/engines/analyzers/saturation_v2/rate_capacity.go`:
   per-bucket service-rate store (running max under backlog, staleness eviction),
   bucket key extended with the **input** dimension, and `rateAnchoredK2()`.
4. **Wiring** — `computeK2` consults the rate-anchored estimator first when the flag
   is on, falling through to the existing chain when it declines to answer. New
   `k2Source` value so the active estimator is visible in logs and metrics.
5. **Tests** — store behaviour (backlog gating, max, decay, eviction), estimator
   arithmetic and clamps, flag-off equivalence with the current path.

## Arrival/completion delay

A completion happens one residence time `W` after the arrival that caused it, so a
completion-derived μ and an instantaneous λ sit on different time bases. During a
ramp, completions still reflect the lighter load of `W` ago, and μ/λ reads as
saturation on a replica that is coping. Occupancy has the same property — it is a
stock that already integrates arrivals over `W`.

λ is therefore smoothed per replica with an EWMA whose time constant is
`W = AvgTTFT + AvgOutputTokens × AvgITL`, both already collected. The weight comes
from the actual gap between samples, so an irregular optimize interval or a missed
cycle does not distort the average; an average older than
`ArrivalSmoothingResetFactor × W` is discarded rather than blended. Without latency
data the rate passes through unsmoothed rather than being averaged by a made-up
constant.

The backlog path needs none of this: it compares no rates, so it is unaffected by
the delay. That is a second reason to keep it ahead of the completions fallback.

## Detector inputs, in order

1. **Backlog** — a queue at least `QueueLengthThreshold` deep. Needs no rates at all,
   which makes it the safety net for a fleet with no EPP and no prior calibration,
   and it is exactly the prefill-heavy case the occupancy estimator misreads as idle.
   A shallower queue does not qualify: arrival jitter produces one at any load.
2. **Arrivals reaching the service rate** — λ from the EPP dispatch rate, smoothed
   over a residence time, against the bucket's μ at `SaturationEnterRatio`. This
   catches the limit before a queue forms.
3. **Completions as λ** — without EPP, and only when there is no queue, completions
   are arrivals. Invalid under backlog, which is why it sits behind that check.

The two `k2Source` labels do **not** identify which of these fired; they distinguish
a limit measured this cycle (`RATE-now`) from one carried over from an earlier one
(`RATE-learned`), which is what the offline replay needs to tell apart. A replica
declines only when nothing has been learned for its bucket and it is not at its limit
now — a state in which nothing is at risk.

## Guards

- Require `KvUsageInstant > 0` and a usable ratio; otherwise decline and fall through.
- `MinRateRatio` floors the ratio a mis-scraped rate can produce; there is no upper
  clamp, since `min(k1, k2)` already bounds the compute estimate by the memory one.
- Both stores prune themselves on insert past `BucketPruneThreshold`, because nothing
  in the engine drives eviction for the analyzer's other stores either — a store that
  relied on being swept would grow unbounded the moment the flag was flipped.
- Never exceed `k1`: `min(k1, k2)` already handles this, but the estimator also
  refuses to return a value below a floor fraction of `k1` to avoid a stalled
  replica collapsing capacity to near zero.

## Known limitations

- **Requires EPP.** `ArrivalRate` comes from the EPP scheduler counter. Without it
  the estimator declines and the existing chain applies unchanged.
- **Needs backlog to calibrate.** A fleet that never queues never learns `μ` and
  falls back to the memory bound — acceptable, since nothing is at risk there, but
  it makes emitting `k2Source` mandatory rather than optional.
- **P/D disaggregation.** `μ` from the generation-tokens histogram is decode-centric;
  a prefill pool would need `rate(vllm:request_prompt_tokens_count[1m])` as its `μ`.
  Not addressed here.
- **Prompt-token throughput is still not collected.** Useful as a diagnostic to
  confirm the prefill-bound reading of the sustained run; not required by the
  estimator.

## Where the two estimators actually differ

Under a deep backlog they agree: the occupancy path records `TokensInUse` as k2 and
the rate path's backlog branch returns the same figure. The divergence is entirely
in the **post-drain** state — queue empty, occupancy collapsed, arrivals unchanged.
There the occupancy path answers from its inflated history and reports abundant
spare capacity (the shed-to-one), while the rate path reads λ still at the ceiling
and holds capacity at the current load. That is the behaviour the cluster legs must
confirm, and it is pinned by a test at the `computeReplicaCapacity` level.

## Validation

1. **Offline** — replay the recorded metrics from the sustained 1000/250 run through
   both estimators and compare `k2` against replica count and queue depth. The
   prediction: the current estimator plateaus high after each queue drain, the
   rate-anchored one tracks `λ/μ` and stays bound.
2. **Cluster** — three legs, KEDA `scaleDown` restored to the shipped
   `Percent 100 / 15s` so the drain cap cannot mask the result, controller restarted
   between legs:
   - sustained 1000/250, flag on — expect the two overshoot-correct cycles to
     disappear;
   - sustained 1000/250, flag off — baseline on the same cluster state;
   - **300/300 steady, flag on — regression control.** A more conservative capacity
     estimate is exactly what could turn "correctly holds at one replica" into
     spurious scale-up. This leg must stay flat at 1 with zero errors.
