# HPA-style stabilization of optimizer recommendations

## Motivation

The optimizer produces a fresh per-variant replica target every optimize cycle
(~30s). Acting on those targets directly is prone to flapping: a momentary load
spike scales up and the next cycle scales straight back down. Today WVA leans on
a downstream HorizontalPodAutoscaler to absorb this — WVA emits a desired-replica
metric and the HPA's own stabilization window damps it.

Stabilizing inside WVA is the prerequisite for WVA ever actuating `/scale`
directly (the 1→N range), because patching the scale subresource without a
stabilization window would lose the damping the HPA provides today and reproduce
the flapping. This change implements that stabilization step; direct actuation is
a separate, later step.

## Design

A small, self-contained `internal/stabilization` package ports the Kubernetes
HorizontalPodAutoscaler **configurable scaling behavior** so an operator can
express damping the familiar HPA way.

- **Config types are reused** from `k8s.io/api/autoscaling/v2`
  (`HorizontalPodAutoscalerBehavior`, `HPAScalingRules`, `HPAScalingPolicy`).
  Already a dependency; maps 1:1 if WVA ever generates HPAs.
- **The algorithm is clean-room.** The upstream behavior logic lives on
  unexported methods of `HorizontalController` in `k8s.io/kubernetes`, which is
  not consumable as a module. The behavior is documented at
  [configurable-scaling-behavior](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/#configurable-scaling-behavior),
  so it is reimplemented rather than imported. Knative's KPA was also considered
  and rejected (wrong domain — it smooths a request/concurrency metric, not a
  replica recommendation — and heavy Knative coupling).

### Algorithm

`Stabilizer.Stabilize` damps one raw recommendation for one scale target. The
pipeline, in order, matches the HPA:

1. **Trailing stabilization window.** Per key, recent raw recommendations are
   retained. The smallest recommendation over the scale-up window is a floor
   (scale up only once every recent recommendation agrees); the largest over the
   scale-down window is a ceiling (scale down only after the window decays). The
   current replica count is clamped into `[floor, ceiling]`. Default windows:
   scale-up `0s` (immediate), scale-down `300s`.
2. **Tolerance deadband.** A change within the configured per-direction
   tolerance fraction of current replicas is suppressed. Default `0` (disabled),
   since the optimizer already decided to act.
3. **Per-period rate policies.** `Pods` and `Percent` policies cap the change
   per `periodSeconds`, budgeting against changes already actuated within the
   period (the period start is reconstructed as `current − added + removed`
   using both scale-up and scale-down events, as the HPA does). `selectPolicy`
   `Max`/`Min`/`Disabled` combines competing policies. Defaults: scale-up the
   higher of `+4 pods` or `+100%` per `60s`; scale-down `-100%` per `60s`. The
   magnitudes and the `300s` down window match the HPA; the `60s` period is
   chosen for WVA's ~30s optimize cadence rather than the controller's `15s`
   default (which would make the budget a near no-op at that cadence).
4. **Min/max clamp** to the variant's bounds.

The `Stabilizer` is long-lived and concurrency-safe; it keeps the trailing
windows and rate budgets in memory, mirroring the HPA controller, which keeps
the same state in memory on the elected leader.

### Wiring

The stabilizer is applied in the V2 engine's `optimizeV2`, immediately after the
optimizer produces decisions (and the `scaling-decision` cycle log) and **before**
the scale-to-zero / minimum-replica enforcer. This ordering is deliberate and
matches the HPA: stabilize the recommendation, then let the hard bounds have the
last word. The existing emit path (Prometheus desired-replica gauge, internal
decision cache, CRD status patch) carries the stabilized value unchanged.

Each variant is keyed `namespace/model/variant[/role]`, so disaggregated
prefill/decode scale targets are damped independently.

A `stabilization-decision` structured log line is emitted per model per cycle,
grouped like the sibling `scaling-decision` line, with `curr`, `raw`
(optimizer recommendation) and `final` (stabilized) per variant.

### Configuration

Gated by `enableStabilization` in the saturation scaling config (default
`false`), mirroring how `enableLimiter` gates the GPU limiter. When disabled the
engine emits the optimizer's raw recommendation exactly as before — no
operator-facing behavior change on upgrade. When enabled, the HPA default
behavior is applied. Exposing the full per-policy behavior through config is a
follow-up.

## Known interactions

- **Double stabilization.** If a downstream HPA also stabilizes the emitted
  metric, lag compounds. The intended contract once this is enabled is that WVA
  owns stabilization and the HPA `behavior` is set to near-passthrough. Until
  that is wired and documented for operators, `enableStabilization` defaults off.
- **Scale-to-zero.** The enforcer runs after stabilization and applies
  scale-to-zero / minimum-replica floors as a hard override, so an idle model
  still scales to zero even if the window is holding replicas high. The trailing
  window records the pre-enforcement recommendation; recovery from zero is
  handled by the scale-from-zero path.

## Testing

`internal/stabilization` has table-driven Ginkgo specs with an injectable clock
covering: immediate default scale-up, the 300s scale-down hold-and-release, the
scale-up window delay, per-period Pods/Percent rate caps, `selectPolicy`
Max/Min/Disabled, the tolerance deadband, min/max clamping, and per-key
isolation.
