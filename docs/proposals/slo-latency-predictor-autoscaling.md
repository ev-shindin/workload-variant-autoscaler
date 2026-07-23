# Proposal: SLO-Driven Autoscaling via the llm-d Latency Predictor

**Authors:** [TBD]
**Status:** Draft
**Created:** 2026-07-23
**Last Updated:** 2026-07-23

---

## Problem Statement

WVA sizes a model by asking an **analyzer** for each variant's capacity. The default `saturation` (V2) analyzer models a per-replica token capacity `min(k1, k2)` from **static, hand-tuned, workload-/hardware-agnostic** thresholds (`KvCacheThreshold`, `QueueLengthThreshold`) that are only *indirectly* tied to any latency SLO — there is no principled way to set them per workload or per TTFT/TPOT percentile.

Meanwhile the inference scheduler (EPP) already runs the **`llm-d-latency-predictor`**: a served ML model, trained online from live traffic, mapping a `(load, request)` feature vector to **p90 TTFT/TPOT**, used to route requests. This proposal reuses it to derive autoscaling capacity anchored to *measured, per-workload, percentile* latency — including prefix-cache and learned batch/memory effects a static model cannot express.

> **Honest framing (load-bearing).** The predictor is trained on *healthy* traffic, so it is **least accurate near saturation** — exactly where the autoscaler operates — and its errors there are **optimistic** (under-predict latency → under-provision → miss SLO). The design therefore: (a) trusts the predictor **only inside its data support**; (b) always runs it **behind the multi-analyzer safety combiner** with static `saturation` as the floor; (c) adds a runtime guardrail. **Boundary safety is *non-regression* (never worse than static), not absolute.** And **pre-seeding, this analyzer ≈ "static `saturation` + a sanity check"** — it earns value only once offline load-test seeding pushes support out to the knee.

## Goals

- Derive **per-variant** capacity (roles via `pod_type`) anchored to predicted TTFT/TPOT (mean or percentile), **inside the predictor's data support**.
- Emit the standard `interfaces.AnalyzerResult`; optimizer/limiter/rescale/scale-from-zero paths unchanged.
- Fail safe to static `saturation` when the predictor is cold, unavailable, out-of-support, or confidently wrong.

## Non-Goals

- Replacing routing; training/serving the predictor; changing the optimizer/limiter; sub-second control.
- **Absolute boundary safety** — the guarantee is non-regression vs. static.
- **Cost right-sizing (fewer replicas than static).** The combiner (max-for-up / unanimous-for-down) means this analyzer only ever *adds* replicas or *blocks* a removal. **Net benefit is better SLO adherence at ≥ static replica cost, not efficiency.** Going below static needs a non-combiner-floored path — unsafe by this design's logic, out of scope.

## Background

**`/predict`** — request features: `kv_cache_percentage`, `input_token_length`, `num_request_waiting`, `num_request_running`, `num_tokens_generated`, `prefix_cache_score`, `pod_type`, `prefill/decode_tokens_in_flight`. Response: `ttft_ms`, `tpot_ms`, `model_type` (Bayesian Ridge / XGBoost / LightGBM), `objective_type` (QUANTILE/MEAN), `quantile`, `last_model_load`.

Properties that **shape** the design:

| Property | Consequence |
|---|---|
| Trained online on healthy traffic | Data-sparse at the boundary; predictions there extrapolate, biased optimistic. The ~5% MAPE is in-regime and does not transfer. |
| Tree or linear, chosen by the predictor operator (another repo) | Trees **flatline** past training range (→ "infinite capacity"); linear under-predicts a superlinear regime. Only linear is cheap to evaluate in-process. |
| Single conditional-quantile point, no uncertainty | Can't express a *marginal* percentile; a p50-trained model vs. a p90 SLO is silently optimistic; no OOD signal exposed. |

**WVA features:** most exist on `ReplicaMetrics` (`KvCacheUsage`, `AvgInputTokens`, `AvgOutputTokens`, `QueueLength`, `PrefixCacheHitRate`). **Gaps:** `num_request_running` is collected only as a constant, **not surfaced** (hard prerequisite); tokens-in-flight and token histograms are absent. Observed `AvgTTFT`/`AvgITL` feed the runtime check.

## Design

Principle: **interpolate inside support, never trust outside.** Each mechanic is *usually*-conservative; the combiner floor does the load-bearing safety.

1. **SLO criterion (marginal, not plug-in).** Feasibility is `E_X[P(L ≤ SLO | X, s)] ≥ p` over the request-size mix. A single-quantile API can't compute this, so evaluate the trained level `q` at a **high size-percentile `x_α`**; this gives `P(L≤SLO) ≥ α·q`, so the **sharp condition is `α·q ≥ p`** (not `α≥p ∧ q≥p` separately — e.g. `α=0.95,q=0.9` ⇒ ≈p85.5). Refuse `q < p`. It's a **usually-conservative heuristic behind the combiner**, not a guarantee (strict needs `q>p` or `α→1`); higher `α`/`q` trade residual optimism for cost. *Upstream ask: distributional/variance output.*

2. **Trust region + static floor (OOD safeguard).** Search only within observed support (`/status` coverage or the feature convex hull). Outside support the analyzer **abstains → static governs via the combiner** — so for a tight SLO whose crossing lies outside support, behavior is **exactly today's**. Coverage/health gating (min samples, quantile model with `q ≥ p`) is a precondition.
   > **Abstain = *omit* the analyzer from the combiner input, not a present entry with a nil `Result`.** `needsScaleDownForRole` treats nil `Result` as a **hard veto on all scale-down**, so a nil-present abstention would silently freeze scale-down for every out-of-support variant, breaking "behaves as today."

3. **Grid scan, not binary search.** Neither model is guaranteed monotone. Scan the coupled trajectory, take the load **one step below** the first up-crossing. Guards: **directional-derivative** `∇f·J` along the trajectory (not any single coefficient's sign; exact only for linear), **multiple-crossing → abstain**, and a **flatline-below-SLO guard** (a tree plateauing under SLO yields no crossing → must not read as infinite capacity). Residual: coarse-grid aliasing for trees — a Phase-1 test case.

4. **On-manifold coupling.** Sweeping `running` propagates to kv% — holding observed low kv% is off-manifold and optimistic. Derive it, with prefix sharing as a **pooled (once-per-batch)** saving, not a per-request discount: `kv_tokens(R) ≈ shared_prefix + R·(input·(1−share) + generated)`. Calibrate the **slope `d(kv%)/dR`** (needs ≥2 operating points), not just the intercept. (The pooled term is O(1), negligible at the boundary — slope accuracy is the real lever, and it's often inestimable pre-seeding.)

5. **Capacity conversion.** `R* → rate` via Little's law using **residence at R\*** (predicted latency ≈ SLO there — a valid fixed point, not idle): `residence ≈ TTFT(R*) + N_out·TPOT(R*)`. Prefer the **token-rate `R*/TPOT`** for the decode axis. Output is `PerReplicaCapacity` in **tokens** (native unit) so downstream is unchanged.

**Integration — combiner-protected (Mode B).** `PredictiveSLOAnalyzer` implements `interfaces.Analyzer`, emits `VariantCapacity{PerReplicaCapacity}` (+ `RoleCapacity` for P/D), and registers **alongside** `saturation`. The combiner (`roleBottleneckReplicas` = max-for-up; `needsScaleDownForRole` = unanimous, `safeRemovalReplicasForRole` = min) means static wins scale-up and gates scale-down — an optimistic predictor **can't** under-provision (it can over-provision → cost). **Mode A** (overwrite `saturation`'s thresholds) is demoted/gated: no second opinion, and `KvCacheThreshold` sets a *token capacity*, not a kv% trigger (a category error). **Seam to audit:** the rescale path reads `saturationEntry(...)` by name, so today it **ignores** this analyzer (cost benefit lands only via scale-up/down) and would be unprotected if a non-saturation analyzer ever became canonical there.

**Runtime guardrail.** **Floor** `PerReplicaCapacity` at `capacityFloorFactor × static` — a **cost bound** (the combiner already prevents going below static; the unbounded direction is over-provisioning from *under*-estimated capacity). **Disable-on-divergence:** predicted vs. observed `AvgTTFT`/`AvgITL` each refresh — an **in-regime shift detector, not a boundary validator** (both agree where healthy traffic lives).

**Model access — WVA hosts the prediction models locally per variant; training stays external.** The predictor has **no features** identifying the model, accelerator, or engine/parallelism/quantization config, so a trained model is implicitly specialized to the exact deployment it learned from — the **variant** (a single per-variant model trained across both P/D roles via `pod_type`; if prefill and decode are *separate* predictor deployments, pull a **per-role** artifact instead — stronger than the doc's own Phase-2 P/D deferral, so treat as an assumption to confirm). WVA **pulls each variant's trained artifact on refresh** from a model registry keyed by variant (**WVA owns no training**) and **evaluates it locally**: **in-process** for Bayesian Ridge/linear (a few coefficients, holds all variants), or a **prediction sidecar in the WVA pod** for trees — the sidecar is point-only, so it substitutes §3's **grid finite-difference** crossing/flatline checks for the `∇f·J` derivative (inheriting the coarse-grid-aliasing residual). Fails safe to static.

> **Critical (Phase-0 blocker): the artifact must carry its own data-support/coverage metadata, co-versioned and swapped atomically with the model.** Local eval severs WVA from the server's `/status`, and WVA's own observed-feature hull ≠ the model's *training* support (seeding pushes support past steady-state traffic — §2b). Without the coverage bundle, §2's OOD trust region **has no signal and safety degrades to the static floor only**. A non-atomic model/coverage swap (model newer than coverage) would make WVA think support is *larger* than it is → evaluate off-support → optimistic under-provision. Missing/stale/skewed → **abstain**.

**Requirements:** (i) a per-variant model **+ coverage registry** (versioned, atomically pulled; "stale" = a version/max-age check); (ii) variant→artifact **discovery** (map a `VariantAutoscaling` to its artifact; refresh on registry version, not `last_model_load` — the artifact path never calls `/predict`); (iii) **multi-model handling** — free in-proc for linear, but the stock server is single-model, so a shared tree sidecar needs an upstream multi-model extension. (Alternatively, query a per-variant prediction Service over HTTP — simpler but a per-cycle network dependency, and an EPP sidecar is pod-local so must be Service-exposed; the remote path *does* retain `/status` coverage.)

```yaml
predictiveSLO:
  enabled: true
  predictor: { modelRegistry: ..., endpoint: ..., modelSource: registry|endpoint, refreshMaxAge: 30m, requireQuantileObjective: true, requireTrainedQuantileGeSLO: true }
  slo: { ttftMs: 500, tpotMs: 40, percentile: 90 }
  criterion: { sizePercentile: 95 }        # target: (sizePercentile/100)*model.quantile >= slo.percentile/100
  guardrail: { capacityFloorFactor: 0.5, divergenceDisableRatio: 1.5 }
  trustRegion: { minBucketSamples: 200 }   # from /status, if exposed
```

## Implementation Phases

- **Phase 0 — gate (nothing builds until it passes).** Land `num_request_running` on `ReplicaMetrics`; **offline load-test/benchmark seeding** to the knee (the hard dependency); **export per-variant coverage/support metadata alongside the model artifact** (the local-eval OOD signal — a blocker); offline eval of predicted vs actual near the knee. **Numeric bar:** e.g. MAPE of predicted p90 latency within load bins ±10% of the SLO crossing **≤ 15% and not sign-biased low**. Fail → stop.
- **Phase 1 — `PredictiveSLOAnalyzer` (Mode B), non-P/D.** Grid-scan + guards (§3); trust region with **abstention**; §1 criterion (`α·q ≥ p`); in-proc BR (or endpoint); capacity floor + divergence-disable; rescale-seam audit.
- **Phase 2 — marginal percentile, P/D, online coupling.** Needs upstream distributional output; token histograms; tokens-in-flight; online slope estimation; `RoleCapacity`.
- **Phase 3 — GA.** Saturation-targeted validation (load tests / canary excursions, not shadow-mode); multi-percentile; per-variant model provenance/lifecycle at scale.

## Alternatives Considered

1. **Keep static thresholds** — the status quo and the safety floor that still governs on abstention.
2. **In-tree white-box capacity model** — no dependency and **no OOD data-gap** (physics extrapolates into the boundary, this proposal's weak spot), but reinvents learned effects and needs separate maintenance. **Choose this proposal when** a seeding harness exists *and* learned nonlinearities matter; **choose the white-box** when boundary behavior dominates and no seeding pipeline exists.
3. **Online-only inline prediction** — highest fidelity, hard control-loop network dependency; deferred.
4. **Reactive scaling on observed SLO breach** — model-free but scales *after* misses.

## Backward Compatibility

Off by default; registers alongside `saturation` and is only overruled by it. Disabled/cold/out-of-support/divergent → exactly today's behavior. `AnalyzerResult` contract unchanged.

## Open Questions

- **Local hosting vs remote:** WVA-local hosting (in-proc eval for linear; a prediction sidecar for trees) needs a per-variant model **artifact registry/download** and, for a shared tree sidecar, **multi-model serving** (`model_id` in the request — upstream); the remote alternative needs a Service-exposed endpoint and a per-cycle network dependency. Granularity is **per variant** either way — the predictor has no model/accelerator/engine-config features, so it is specialized to the exact variant it trained on.
- **Seeding ownership** (§2/Phase 0) — WVA CI, predictor repo, or shared harness?
- **`/status` coverage exposure** — unverified; if absent, joins the upstream asks alongside predictive variance.
- **Distributional/variance output**; **monotone-constrained training** — upstream asks.
- **Coverage-with-artifact** export + atomic co-versioning (the local-eval OOD signal — Phase-0 blocker); **P/D**: one per-variant model across roles vs. separate per-role artifacts; **rescale seam** protection.

## References

- `llm-d-latency-predictor` — `prediction/prediction_server.py`, `common/types.py`.
- llm-d blog — *Predicted-Latency Based Scheduling for LLMs*: https://llm-d.ai/blog/predicted-latency-based-scheduling-for-llms
- WVA — `internal/interfaces/analyzer.go`; combiner `internal/engines/pipeline/analyzer_helpers.go`; `internal/engines/analyzers/saturation_v2/analyzer.go`; `internal/config/saturation_scaling.go`.
