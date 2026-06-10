# Proposal: Pod → Autoscaling-Object Mapping via Per-Cycle Derivation

## User story

A platform engineer has a `Deployment` of vLLM replicas (or a `LeaderWorkerSet`) **already running in production** and wants WVA to autoscale it. They follow the llm-d guide and apply its autoscaling kustomization for the workload.

To make it work, the recipe also requires editing the workload's **pod template** to add `llm-d.ai/variant: <name>` — on a `LeaderWorkerSet`, in *both* `leaderTemplate` and `workerTemplate` — plus a `metricRelabelings` rule on the `ServiceMonitor`.

Editing the pod template of a running workload is a **rolling restart of the whole fleet** — forced purely to *adopt* autoscaling, before it scales anything. And the label is a hand-maintained join key: get its key, its value, the LWS double-template, or the relabel rule wrong, and WVA silently falls back to wrong scaling, with no error.

## Problem

WVA maps every per-pod vLLM metric (KV-cache usage, queue depth, latency, cache config) to the autoscaling object that manages the pod — on the scaling hot path. Today (PR #1145) that mapping is **tenant-driven**: the pod-template label above, joined through a `ServiceMonitor` relabel rule.

**The core cost is adoption.** Turning on autoscaling for an existing workload shouldn't touch the workload — but the join key lives in the pod template, so adopting WVA forces a rolling restart of a running fleet before autoscaling does anything. No other autoscaler imposes this: HPA, KEDA, and VPA attach via a target reference and read the target; none edit the pod template.

The same hand-maintained join key carries two more problems:

- **It fails silently.** A wrong label key, a value drifted from the expected name, a missing relabel rule, or a half-templated LWS → zero matching metrics → the collector falls through to `computeReplicaCapacityFallback` (the path the `cache_config_info` bug surfaced in #1198) and produces "working" but wrong scaling, with no signal.
- **The anchor is being deprecated.** The label's value must equal the `VariantAutoscaling` name — the very CRD being removed (`deprecate-va-crd.md`) — so the contract already points at a referent that's going away.

Issue #1072 names the same fragility but proposes the *opposite* fix — have the controller stamp the label onto pod templates — which trades tenant friction for controller-driven rolling restarts and mutation of tenant-owned resources (see Alternatives).

## Solution

WVA derives the mapping itself from the scale target it already reads, once per optimization cycle (the timer-driven loop that already scrapes pods, every `OptimizationInterval` — not the per-VA-event `Reconcile`) — **no tenant-authored join key, no pod mutation, no new watch.**

- **Per-cycle derivation.** Once per cycle, for each autoscaling object it manages (a `VariantAutoscaling` today), WVA reads the `scaleTargetRef`, lists the pods behind the target's selector, and maps each pod back to that object. It's kind-aware for the standard kinds — `Deployment` and `LeaderWorkerSet` expose selectors and owner chains differently — with the opt-in label (below) for kinds the selector can't resolve. Resolution moves from per-metric to once-per-cycle.
- **Ownership.** A pod is attributed by selector + namespace match. Confirming strict ownership would add one read up the owner chain (`Pod → ReplicaSet → Deployment`, or `Pod → StatefulSet → LeaderWorkerSet`); since a workload's selector is meant to uniquely own its pods, the default trusts selector + namespace and treats that extra read as optional hardening for the overlapping-selector edge case.
- **No new watch.** The per-cycle list reuses the pod LIST the scraping source already issues (`pod_scraping_source.go` already `List`s the pods it scrapes), so it adds **no always-on Pod watch** and no cluster-scale Pod cache; WVA's controller already watches the `VariantAutoscaling` and its scale target, and neither watches `Pod`.
- **O(1) hot path.** Each per-metric lookup becomes a single `Lookup(podKey) → autoscaling object` map read (`podKey` = `namespace/name`, or `:port` for multi-vLLM pods); no Kubernetes calls in the metrics path.
- **Misses become a signal, not silence.** An unresolved metric increments `wva_pod_mapping_miss_total{reason}` and logs a structured warning; an autoscaling object whose pods exist but aren't resolving gets a `PodMappingMissing` status condition. The old silent wrong-capacity fallback becomes visible. (Most misses are the brief pod-creation-to-next-cycle window — transient and self-healing.)
- **Opt-in label for kinds the selector can't resolve.** For workloads WVA can't attribute by selector — custom CRDs, or non-standard owner chains — a label stays as a *higher-precedence override*. Its value names the same **autoscaling object** the derived path resolves to, not a frozen VA name, so it tracks the deprecation migration (below) instead of re-introducing the anchor the join key fails on. WVA reads it, never writes it; the existing `llm-d.ai/variant` label and its ServiceMonitor relabel rule (#1145) stay for installs that keep them.

**Tenant experience:** apply the llm-d autoscaling kustomization for a `Deployment`/`LWS` — done. No pod-template edit, no relabel rule, no LWS double-template, no rolling restart to adopt.

The map exposes one internal contract — `Lookup(podKey) → autoscaling object`. That object is a `VariantAutoscaling` today; after VA-CRD deprecation it's an annotated `ScaledObject`/`HPA`. The contract is stable across that migration, so it lands without consumer churn.

## Alternatives considered

1. **Status quo — tenant label + ServiceMonitor relabel rule (#1145 as merged).** Achieves O(1) lookup by demanding two coordinated authoring artifacts plus a rolling restart to adopt. The silent-failure and in-place-adoption costs are the motivation here.
2. **Controller stamps the label on pod templates (#1072).** Zero tenant authoring, but mutates tenant-owned `Deployment`/`LWS` (rolling restart on first reconcile, permanent GitOps drift) and conflicts with the "WVA reads, never writes" persona of the config-UX proposal. Both this proposal and #1072 carry kind-specific logic for the standard kinds; the difference is **reads vs writes** — derivation reads selectors and owner-refs and mutates nothing, while #1072 writes pod templates, forcing rollouts and GitOps drift. Same kind surface, opposite blast radius.
3. **Revert #1145.** Would also undo multi-vLLM-per-pod support, the relabel infrastructure, and test/doc work this proposal keeps. This is a corrective follow-up layered on #1145, not a revert.
4. **Validating webhook / admission policy on the pod template.** Turns one silent failure loud at apply time, but leaves the *requirement* (and the rollout-to-adopt cost) in place; CEL is single-object, so it can check the label is present but not that its value equals a VA name; and it validates the very VA-name anchor that VA-deprecation removes. Useful as defense-in-depth for the opt-in label, not a substitute for deriving the mapping.
5. **External sidecar / DaemonSet that watches pods and stamps the label.** Adds an operational component to do what the controller already can. Rejected on complexity.

## Open question

**List scope and cost.** The map is built from a per-cycle pod LIST scoped by the `scaleTargetRef` selector — no always-on watch. Open: whether to additionally restrict the list to namespaces that contain a managed workload, and whether the per-cycle LIST cost holds at scale. Settled with a list-latency benchmark on a representative cluster.

---

*Out of scope here (tracked separately): migration steps, implementation phasing, and the full property-by-property comparison — all mechanical once the boundary above is agreed. This proposal is a corrective follow-up to #1145 and explicitly does **not** revert it or adopt controller-side pod mutation (#1072).*
