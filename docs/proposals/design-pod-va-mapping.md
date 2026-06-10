# Proposal: Pod → `VariantAutoscaling` Mapping via Per-Cycle Derivation

## User story

A platform engineer has a `Deployment` of vLLM replicas (or a `LeaderWorkerSet`) **already running in production**. They want WVA to autoscale it, so they create a `VariantAutoscaling` pointing at the workload via `scaleTargetRef` — exactly how they'd attach an HPA — and expect it to work.

It doesn't. WVA reports a desired-replica count, but it's quantitatively wrong, with **no error surfaced**. Digging in, they discover they also had to:

1. add `llm-d.ai/variant: <VA-name>` to the workload's **pod template** — forcing a **rolling restart** of the whole workload (and a GitOps source edit) just to start autoscaling — and on a `LeaderWorkerSet`, to *both* `leaderTemplate` and `workerTemplate`; and
2. add a `metricRelabelings` rule to their `ServiceMonitor`.

Get the label key, its value (which must equal the VA name exactly), the LWS double-template, or the relabel rule wrong, and they're silently back to wrong scaling.

They expected "point an autoscaler at a workload." They got "hand-maintain a join key for the controller, restart your pods to adopt, and know Prometheus relabeling."

## Problem

WVA must map every per-pod vLLM metric (KV-cache usage, queue depth, latency, cache config) to the `VariantAutoscaling` that owns the pod — it sits on the scaling hot path. Today (PR #1145) that mapping is **tenant-driven** (the label + relabel rule above). That's wrong for an autoscaler in three ways:

- **It's not how autoscalers attach.** HPA, KEDA, VPA, and the Cluster Autoscaler bind via `scaleTargetRef` and read the target — none require editing the pod template. WVA already takes a `scaleTargetRef`; the required label is the one thing that breaks the norm.
- **It blocks in-place adoption.** Pointing WVA at a running `Deployment` means editing `spec.template.metadata.labels` first — a pod-template change, i.e. a rolling restart, *before autoscaling even works*.
- **It's a hand-maintained join key, and every part fails silently.** A wrong label key, a value drifted from the VA name, a missing relabel rule, a half-templated LWS → zero matching metrics → the collector falls through to `computeReplicaCapacityFallback` (the path the `cache_config_info` bug surfaced in #1198) and produces "working" but wrong scaling, with no signal. Worse, the value's anchor — the VA name — is being deprecated (`deprecate-va-crd.md`), so the contract already points at a referent that's going away.

Issue #1072 names the same fragility but proposes the *opposite* fix — have the controller stamp the label onto pod templates — which trades tenant friction for controller-driven rolling restarts and mutation of tenant-owned resources (see Alternatives).

## Solution

WVA derives the mapping itself from the scale target it already reads, once per optimization cycle (the timer-driven loop that already scrapes pods, every `OptimizationInterval` — not the per-VA-event `Reconcile`) — **no tenant-authored join key, no pod mutation, no new watch.**

- **Per-cycle derivation.** Once per cycle, for each managed `scaleTargetRef`, WVA lists the pods behind the target's selector and resolves each to that target's `VariantAutoscaling`. This is kind-aware for the standard kinds — a `Deployment`'s and a `LeaderWorkerSet`'s selectors and owner chains differ — with the opt-in label (below) covering kinds the selector can't resolve. Ownership is confirmed by walking the pod's owner chain back to the scale target (for a `Deployment`, pod → `ReplicaSet` → Deployment), so a stray pod merely sharing the selector isn't misattributed — the *depth* of that walk is an open question. Resolution moves from per-metric to once-per-cycle.
- **No new watch.** The per-cycle list reuses the pod LIST the scraping source already issues (`pod_scraping_source.go` already `List`s the pods it scrapes), so it adds **no always-on Pod watch** and no cluster-scale Pod cache; WVA's controller already watches the `VariantAutoscaling` and its scale target, and neither watches `Pod`.
- **O(1) hot path.** Each per-metric lookup becomes a single `Lookup(podKey) → VA` map read (`podKey` = `namespace/name`, or `:port` for multi-vLLM pods); no Kubernetes calls in the metrics path.
- **Misses become a signal, not silence.** An unresolved metric increments `wva_pod_va_map_miss_total{reason}` and logs a structured warning; a VA whose pods exist but aren't resolving gets a `PodMappingMissing` status condition. The old silent wrong-capacity fallback becomes visible. (Most misses are the brief pod-creation-to-next-cycle window — transient and self-healing.)
- **Opt-in label for the long tail.** `llm-d.ai/variant` stays as a *higher-precedence override* for custom workload kinds / non-standard topologies the selector can't resolve. WVA reads it, never writes it; the ServiceMonitor relabel rule stays for installs that keep it.

**Tenant experience:** author a `VariantAutoscaling` pointing at a `Deployment`/`LWS` — done. Same shape as an HPA. No required label, no relabel rule, no LWS double-template, no rollout to adopt.

The map exposes one internal contract — `Lookup(podKey) → discovery key`. The key *means* a `VariantAutoscaling` today; after VA-CRD deprecation it means "the discovery source for this pod" (an annotated `ScaledObject`/`HPA`). Same API, different value source — so the discovery migration lands without consumer churn.

## Alternatives considered

1. **Status quo — tenant label + ServiceMonitor relabel rule (#1145 as merged).** Achieves O(1) lookup by demanding two coordinated authoring artifacts plus a rolling restart to adopt. The silent-failure and in-place-adoption costs are the motivation here.
2. **Controller stamps the label on pod templates (#1072).** Zero tenant authoring, but mutates tenant-owned `Deployment`/`LWS` (rolling restart on first reconcile, permanent GitOps drift) and conflicts with the "WVA reads, never writes" persona of the config-UX proposal. Both this proposal and #1072 carry kind-specific logic for the standard kinds; the difference is **reads vs writes** — derivation reads selectors and owner-refs and mutates nothing, while #1072 writes pod templates, forcing rollouts and GitOps drift. Same kind surface, opposite blast radius.
3. **Revert #1145.** Would also undo multi-vLLM-per-pod support, the relabel infrastructure, and test/doc work this proposal keeps. This is a corrective follow-up layered on #1145, not a revert.
4. **Validating webhook / admission policy on the pod template.** Turns one silent failure loud at apply time, but leaves the *requirement* (and the rollout-to-adopt cost) in place; CEL is single-object, so it can check the label is present but not that its value equals a VA name; and it validates the very VA-name anchor that VA-deprecation removes. Useful as defense-in-depth for the opt-in label, not a substitute for deriving the mapping.
5. **External sidecar / DaemonSet that watches pods and stamps the label.** Adds an operational component to do what the controller already can. Rejected on complexity.

## Open questions

- **List scope and cost.** The map is built from a per-cycle pod LIST scoped by the `scaleTargetRef` selector — no always-on watch. Open: whether to additionally restrict the list to namespaces that contain a `VariantAutoscaling`. Settled with a list-latency benchmark on a representative cluster.
- **Owner-ref confirmation depth.** Whether confirmation must read the `ReplicaSet` (pod → `ReplicaSet` → Deployment) or can stop at the pod's direct owner. This is a *correctness* question, not a cost one: a `Deployment`'s pod is directly owned by its `ReplicaSet`, so stopping there confirms pod ∈ `ReplicaSet`, not pod ∈ Deployment. Settled by whether two scale targets can present selector-overlapping pods that the shallow check would misattribute — analysis, not a benchmark.

---

*Out of scope here (tracked separately): migration steps, implementation phasing, and the full property-by-property comparison — all mechanical once the boundary above is agreed. This proposal is a corrective follow-up to #1145 and explicitly does **not** revert it or adopt controller-side pod mutation (#1072).*
