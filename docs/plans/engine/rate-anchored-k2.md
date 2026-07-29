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

Add a second, **rate-anchored** estimator for `k2`, selected by an internal flag,
leaving the existing estimator in place and untouched when the flag is off.

When a replica has a backlog, its completion rate *is* its service rate — this
holds whichever resource binds (prefill compute, decode bandwidth, memory), which
is what makes it the right anchor for a workload-agnostic capacity model:

```
μ  = max completion rate observed for this workload bucket while QueueLength > 0
λ  = this replica's arrival rate (EPP dispatch rate)
k2 = KvUsageInstant × TotalKvCapacityTokens × (μ / λ)
```

At `λ = μ` the replica is exactly saturated and `k2` equals its current occupancy,
so utilization reads 100% **even at 16% KV**. At `λ = μ/2`, `k2` is twice the
occupancy and utilization reads 50%. The result stays in KV tokens, so `min(k1, k2)`,
`spareCapacity`, and every downstream consumer are unchanged.

Properties that matter here:

- **Blind to which resource binds.** No assumption that the workload is decode-heavy.
- **Monotone in the right direction.** `μ` is a ceiling measured only under backlog;
  heavier load cannot inflate it. The current estimator moves the wrong way.
- **Does not need a live queue.** Once `μ` is known for a bucket it stays valid
  until the workload shape changes, unlike an occupancy sample which is meaningful
  only at the instant it was taken.
- **Uses the instantaneous KV reading**, not the 1-minute peak. The peak stays where
  it belongs — on the demand side, where erring high is deliberate.

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
| occupancy (no peak bias) | `vllm:kv_cache_usage_perc` (instant) | `KvUsageInstant` | throughput-analyzer only |
| queue | `max_over_time(vllm:num_requests_waiting[1m])` | `QueueLength` | unconditional |
| KV capacity | `vllm:cache_config_info` | `TotalKvCapacityTokens` | unconditional |

No new PromQL. `QueryRequestRate` and `QueryKvUsageInstant` move to a shared
registrar called unconditionally by the saturation engine; the throughput analyzer
registers the same two only if absent, so either order works and neither panics.

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

## Lambda fallback chain

`saturationRatio` takes the most direct signal available:

1. **EPP arrival rate** (`k2SrcRateAnchored`) — the intended path. λ is measured
   independently of the engine, so μ/λ holds whether or not the replica keeps up.
2. **Backlog** (`k2SrcRateBacklog`) — a queueing replica is saturated by
   observation, so the ratio is 1 and capacity is the current occupancy. Needs
   neither λ nor a calibrated μ, which makes it the safety net for a fleet with no
   EPP and no prior calibration — and it is exactly the prefill-heavy case the
   occupancy estimator misreads as idle.
3. **Completions as λ** (`k2SrcRateNoEPP`) — with no queue, everything arriving is
   served within the scrape window, so completions are arrivals. Valid only in that
   case, which is why it sits behind the backlog check.

Only an idle, never-calibrated replica with no EPP declines, and there is nothing
at risk in that state. Each source has its own `k2Source` label (`RATE-λ`,
`RATE-q`, `RATE-c`) so the active path is visible per replica.

## What the capacity store learns

`capacityStore` feeds zero-replica estimation, cross-variant `FindCompatible`
lookups and the fallback path — all of which need a **ceiling**, what a replica can
do. The rate-anchored value is operating-point-relative, so it is persisted only
when it is ceiling-like:

- **λ ≥ μ** — the replica is at or past saturation, so `occupancy × μ/λ` is what it
  actually held. Persisted.
- **λ < μ** — the same expression is occupancy scaled by headroom. Correct for this
  cycle's utilization, not a property of the variant. The store keeps the
  occupancy-based estimate instead; persisting the headroom-scaled value would
  under-state capacity for a variant that later scales from zero.

`computeReplicaCapacity` therefore tracks `effectiveCapacity` (what this cycle
scales on) separately from `storedCapacity` (what the store learns).

## Guards

- Require `KvUsageInstant > 0` and a usable ratio; otherwise decline and fall through.
- Clamp `μ/λ` to `[0.05, 20]` so a mis-scraped rate cannot swing capacity wildly.
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
