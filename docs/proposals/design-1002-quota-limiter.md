# Issue #1002 — Quota-based limiter: design proposal

## 1. Problem statement

The current `TypeInventory` derives accelerator caps exclusively from cluster node discovery. Operators have no way to:

- Cap WVA's own usage to reserve headroom for other workloads — e.g., a cluster has 32× H100 but inference may use at most 16, leaving 16 for training. The cap *is* the reservation; there is no separate reservation primitive.
- Divide capacity across tenants/teams, with the Kubernetes namespace as the team boundary — e.g., `team-a ≤ 8× H100`, `team-b ≤ 4× H100`, no cross-team borrowing.
- Cap GPU types independently — e.g., `H100 ≤ 16`, `A100 unlimited`, in the same cluster.
- Enforce contractual / governance limits decoupled from physical inventory. (Chargeback/billing is orthogonal and out of scope — see Non-goals.)
- Run WVA in clusters where the operator's ServiceAccount cannot list `nodes` cluster-wide (managed K8s with locked-down RBAC, multi-tenant clusters).

The acceptance criteria from #1002 frame this as "operator-declared GPU caps at cluster and/or namespace scope, enforced by the WVA scaling pipeline."

This proposal evaluates the alternatives — including native Kubernetes primitives — before recommending an implementation path.

## 2. Goals and non-goals

**Goals.**
- Cap GPUs per accelerator type at cluster and/or namespace scope, declared by the operator.
- Enforce the cap at scaling-decision time so WVA never *requests* more replicas than allowed. Precisely, the guarantee is: **WVA never sets a desired replica count whose cumulative per-type GPU request would exceed the cap**, so WVA's own scaling decisions never create quota-exhaustion `Pending` pods. It is *not* a guarantee that pods never go `Pending` for non-quota reasons (another tenant consuming shared capacity, or physical exhaustion below the quota) — `min(physical, quota)` bounds those but cannot eliminate them.
- Compose cleanly with the existing `TypeInventory` so physical capacity remains an upper bound (sub-issue #1003). `TypeInventory` is re-read from node discovery each cycle, so `min(physical, quota)` tracks node add/drain automatically — the quota only ever lowers the live physical bound.
- Support an explicit "unlimited" sentinel and an "exclude this namespace" affordance for system tenants. An excluded namespace is *not capped*, but its GPU usage still counts against physical capacity — exclusion removes the *cap*, not the *accounting*, so the cluster aggregate cannot overcommit (mechanism in §3.A).
- Work on any conformant Kubernetes distribution — no OpenShift-only or vendor-only dependencies.
- Provide a clear DecisionStep trace when the cap binds.

**Non-goals.**
- Job admission, queueing, or fair-share scheduling. WVA scales long-running Deployments / LWS, not batch jobs.
- Per-pod resource accounting, billing, or chargeback ledger. Other tools own that.
- Replacing or duplicating Kubernetes `ResourceQuota` — we will read from it if available (option A2, §3).
- Cluster autoscaler-level decisions. WVA assumes nodes already exist; provisioning is upstream.
- Multi-cluster federation. One controller, one cluster.

## 3. Options considered

Options **A** and **A2** below are the **proposed design** (recommended in §5); **B–H** were considered and are rejected or judged orthogonal.

### A. WVA-internal `QuotaLimiter` (operator-declared per-GPU-type caps)

Operator-declared YAML/ConfigMap, parsed into `QuotaLimiterEntries`, enforced via a new `QuotaInventory` (implements `Inventory` + `NamespaceAwareInventory`) inside the existing limiter pipeline. This is **not** a duplicate of `ResourceQuota`: it expresses per-GPU-*model* caps (`H100` vs `A100`) enforced at *decision* time, which `ResourceQuota` cannot (a single `requests.nvidia.com/gpu` counter, enforced at admission). Where a `ResourceQuota` already exists, WVA reads it via A2 rather than redeclaring.

| Dimension | Detail |
|---|---|
| Scope | Cluster or namespace, both can coexist as separate entries |
| Granularity | Per accelerator type (`H100`, `A100`, …); `-1` = unlimited |
| Enforcement point | WVA scaling decision (per cycle, before requesting replicas) |
| Operator UX | Declared on the cluster-default `ScalingPolicy` (`spec.quota`) — the single config surface from #1194 (see §6.1); the prototype's ConfigMap is an internal/test detail, not a shipped surface |
| Multi-tenancy | `default` key as per-namespace fallback (LimitRange-style) |
| Failure mode | Decision capped at quota; emits `DecisionStep` with `limited by quota[…]` |
| Dependencies | Zero new operators / CRDs / admission webhooks |
| Status | Prototype on `feat/quota-limiter`, 8 commits, 50+ Ginkgo specs |

**Within a cycle, quota is a snapshot.** WVA reads the cap **once at the start of each optimize cycle** and decrements it in memory as it allocates across models, so every model in that cycle draws from one consistent budget — two models cannot both claim the last GPU. It does **not** re-read quota from the API between per-model decisions within a cycle (that would be racy and order-dependent); the next cycle re-reads the fresh cap. This is the same per-cycle snapshot discipline `TypeInventory` already uses for physical capacity. When a cluster aggregate spans namespaces, the *order* in which models draw it down within the cycle is the contention case discussed in §6.3.

**Physical accounting (and excluded namespaces).** The physical bound the quota composes with is `Available = allocatable − Used`, where `Used` comes from `DiscoverUsage` (`k8s_with_gpu_operator.go`) — a sum of GPU requests across **all** pods cluster-wide, with no namespace filter. So a namespace *excluded* from the quota still has its pods counted against available physical, and the cluster aggregate cannot overcommit. This holds wherever physical discovery is available; in quota-only mode (no readable nodes/pods, §1) quota is the sole bound by design, so there is no physical figure to overcommit against.

### A2. WVA `ResourceQuotaReader` (WVA consumes K8s-native `ResourceQuota`)

Distinct from option B below. Here WVA *actively reads* `kind: ResourceQuota` objects via a controller-runtime informer and surfaces the effective per-namespace `requests.<resource>` caps as a `ConstraintProvider` input to the limiter chain. The admin authors standard K8s `ResourceQuota` YAML; WVA picks it up automatically.

| Dimension | Detail |
|---|---|
| Scope | Namespace only (matches `ResourceQuota` shape; cluster scope not natively possible without OCP `ClusterResourceQuota`) |
| Granularity | Per-extended-resource (e.g., `nvidia.com/gpu` as a single counter — same per-GPU-model limitation as classic `ResourceQuota`) |
| Enforcement point | WVA scaling decision (the cap is read from K8s but applied at decision time, *not* at pod admission) |
| Operator UX | Standard `kubectl apply -f resourcequota.yaml` — zero new schema for the admin |
| Multi-tenancy | Native K8s namespace boundary |
| Failure mode | Decision capped at quota; emits `DecisionStep` with `limited by resourceQuota[ns=…, resource=…]` |
| Dependencies | Controller-runtime informer on `ResourceQuota` objects |
| Status | Not yet implemented |

The key idea: A2 separates *policy* (where the cap is declared — by admins in `ResourceQuota`) from *enforcement* (where it binds — in WVA's decision loop). This sidesteps the load-bearing problem with option B (admission-time enforcement leaving WVA blind to the cap) while preserving the K8s-native UX.

A2 does not solve per-GPU-model granularity — it inherits the single-counter limitation of `requests.nvidia.com/gpu`. But for the platform-admin persona who already governs the cluster with `ResourceQuota` and either runs a homogeneous GPU pool or doesn't need per-model splits, A2 is the right answer. (#1227 implements exactly this reader's operator-free-capacity kernel; the convergence path is to land it as the A2 `ConstraintProvider` here, composing via the limiter chain, rather than as a parallel limiter that replaces node discovery.)

### B. Classic Kubernetes `ResourceQuota` alone (admin-managed, WVA-unaware)

Distinct from A2 above. Here the admin authors `kind: ResourceQuota` and **WVA does nothing differently** — WVA continues to make scaling decisions without knowing the cap exists. Enforcement happens entirely at pod admission by the kube-apiserver admission controller.

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: team-a-gpu
  namespace: team-a
spec:
  hard:
    requests.nvidia.com/gpu: "8"
```

| Strengths | Weaknesses |
|---|---|
| K8s-native; familiar to every operator. | **No per-GPU-model granularity.** `nvidia.com/gpu` is a single extended-resource counter — H100 and A100 count against the same bucket. `scopeSelector` cannot filter by node label (e.g., `nvidia.com/gpu.product=H100`); the supported scope operands are limited to `PriorityClass`/`BestEffort`/`NotBestEffort`/`Terminating`/`NotTerminating`/`CrossNamespacePodAffinity`. The NVIDIA k8s-device-plugin does not advertise per-model extended resources for full GPUs ([NVIDIA/k8s-device-plugin#424](https://github.com/NVIDIA/k8s-device-plugin/issues/424), open since 2023 — a design for per-SKU resource naming is in progress, but the single-counter gap stands today). The AMD and Intel device plugins follow the same single-counter pattern. **MIG and time-slicing are exceptions** — `nvidia.com/mig-1g.5gb`, `nvidia.com/gpu.shared`, etc. are distinct extended resources and *can* be quota'd separately — but those are slice-size or sharing-mode discriminators, not GPU-model discriminators. |
| Enforced by the API server, can't be bypassed. | **Per-namespace only.** No cluster-wide aggregate (see option C). |
| Already integrates with `LimitRange` for defaults. | **Enforcement is at pod admission, not at scaling decision.** WVA would scale up, K8s would reject the new pods, WVA would see a stuck rollout. Late binding. |
| Works on every conformant Kubernetes. | No "exclude this namespace from the cap" affordance. |
| Operators can already use it today, no WVA change needed. | Doesn't surface in WVA's decision trace; operators see admission failures, not capped scaling decisions. |

**Verdict:** Necessary but not sufficient. Useful as a K8s-layer hard ceiling for safety, but cannot express per-GPU-model caps that #1002 requires.

### B2. Kubernetes Dynamic Resource Allocation (DRA)

DRA is the modern replacement for device-plugin-based extended resources. **GA in Kubernetes 1.34** (`resource.k8s.io/v1`). Devices are exposed by a *DRA driver* (e.g., the NVIDIA DRA driver at `kubernetes-sigs/dra-driver-nvidia-gpu`); admins author `DeviceClass` objects with CEL selectors that pick devices by attribute (product name, MIG profile, memory, …); workloads consume devices through `ResourceClaim` objects.

Critically for #1002: **`ResourceQuota` supports DRA via a per-DeviceClass syntax.** From the upstream docs:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: team-a-gpu
  namespace: team-a
spec:
  hard:
    h100.example.com.deviceclass.resource.k8s.io/devices: "8"
    a100.example.com.deviceclass.resource.k8s.io/devices: "4"
```

Per-model quotas drop out naturally if the admin defines a `DeviceClass` per model.

| Strengths | Weaknesses |
|---|---|
| **Per-GPU-model quotas are first-class.** Authoring a DeviceClass per model is the recommended NVIDIA DRA pattern. | **Requires Kubernetes ≥ 1.34** for GA. Earlier clusters get nothing or an alpha-quality experience. WVA targets a broader version floor. |
| K8s-native; long-term direction of the ecosystem. | **The *improved* DRA quota is still maturing.** The basic per-`DeviceClass` device-*count* quota (`<class>.deviceclass.resource.k8s.io/devices`) ships with DRA GA in 1.34. But the allocation-time quota — capping e.g. total GPU *memory* rather than device count, and avoiding over-rejection under contention — is [KEP #4840](https://github.com/kubernetes/enhancements/issues/4840), still in progress as of mid-2026. |
| Same enforcement-via-admission-controller story as classic `ResourceQuota`. | **Same late-binding problem as classic ResourceQuota.** Enforced at pod admission, not at WVA's decision time. WVA would still scale up and see stuck rollouts. |
| Works alongside the existing device plugin via the Extended-Resource ↔ DRA bridge (alpha). | Operator burden: author DeviceClasses, run a DRA driver, manage `ResourceClaim` lifecycle. |
| | Same observability gap as B — quota violations appear as pod-admission failures, not in WVA's decision trace. |

**Verdict:** The right long-term direction for K8s-native per-GPU-type quotas, but not a substitute for #1002 in the WVA decision pipeline today. WVA should plan to integrate with DRA in a future phase (read DeviceClass-scoped quotas as additional chain inputs, alongside any `ResourceQuotaReader`). It is kept as a full option here — rather than dropped to a footnote — because it is the K8s-native per-type path reviewers will ask about and the basis for the future DRA `ConstraintProvider` named in §5; the recommendation explains why it is not the *current* mechanism.

### C. OpenShift `ClusterResourceQuota`

OpenShift adds `ClusterResourceQuota` (CRQ) on top of `ResourceQuota`. Selects namespaces by label and applies an aggregate cap across them. Closes the "no cluster-wide scope" gap of `ResourceQuota`.

| Strengths | Weaknesses |
|---|---|
| Cluster-wide aggregate, label-selected. | **OpenShift-only.** Not available on vanilla K8s, GKE, EKS, AKS. |
| Same per-GPU-type limitation as `ResourceQuota`. | Same late-binding (admission-time) enforcement. |
| Same operator UX as `ResourceQuota`. | Same no-exclude. |

**Verdict:** Wrong platform binding. We want WVA to work on any conformant K8s.

### D. Kueue

[Kueue](https://kueue.sigs.k8s.io/) is the SIG-scheduling Kubernetes-native job queueing system. `ClusterQueue` + `LocalQueue` + cohort-based fair sharing. `ResourceFlavor` objects can key on arbitrary node labels including `nvidia.com/gpu.product`, so per-GPU-model quotas (e.g., 8× H100 and 4× A100 in one ClusterQueue) are expressible — this is the most widely deployed workaround for the B/B2 gaps today on clusters that haven't yet adopted DRA. Its serving-workload support is recent and `v1beta2`: elastic Deployment/ReplicaSet admission arrived via the WorkloadSlices feature (v0.13.0), while `StatefulSet` scale up/down through Kueue is explicitly unsupported. Either way Kueue acts at *admission* — it decides whether a replica may run, not how many replicas to run; the replica-count decision stays with WVA.

| Strengths | Weaknesses |
|---|---|
| **Per-GPU-model quotas via `ResourceFlavor` work today** on any K8s version, by selecting on the device plugin's node labels. | **Different enforcement layer.** Kueue now manages `Deployment`/`StatefulSet`/`LWS`, but it gates **pod admission** against quota, whereas WVA decides **replica count**. Replicas WVA requests beyond Kueue's quota are created-then-held `Pending` — see the *anticipated-capacity trap* in the Verdict. |
| Cohort fair-share and borrowing — far richer than what #1002 asks for. | Adds a new controller, new CRDs, new admission webhook. Operational overhead. |
| Designed for AI/ML workloads. | Kueue inserts an admission gate between "WVA decides scale" and "K8s schedules pod," so WVA's requested replica count and what actually runs diverge — the basis of the anticipated-capacity trap (Verdict). |

**Verdict:** Not the enforcement mechanism for #1002, but the right *source* to read where it is deployed. Because Kueue enforces at admission, using it alone produces the **anticipated-capacity trap**: replicas WVA requested beyond quota sit `Pending` (gated by Kueue), WVA folds them into `anticipatedSupply` — `saturation_v2/analyzer.go:97` computes `anticipatedCapacity = (ReplicaCount + PendingReplicas) × PerReplicaCapacity`, and `:119` sets `requiredCapacity = totalDemand/ScaleUpThreshold − totalAnticipatedSupply`. Because a gated pod is *created* (phase `Pending`) it counts in `PendingReplicas`, so `requiredCapacity` is suppressed and the optimizer won't scale a sibling variant on a different `ResourceFlavor` that could serve the same `InferencePool` — the pool stays under-served while WVA believes it is converging. The fix is decision-time enforcement: WVA *reads* Kueue's `ResourceFlavor`/`ClusterQueue` quota as a `ConstraintProvider` (the same reader family as A2's `ResourceQuotaReader`) and caps the request, so it never asks for replicas Kueue would gate. Where an org already runs Kueue, that reader reuses its per-GPU-type quota directly; where it does not, Option A provides the same cap with no new operator.

### E. Hierarchical Namespaces (HNC) / Capsule

**Orthogonal — not a substitute, omitted from the matrix.** Tenancy frameworks that nest namespaces and inherit `ResourceQuota`; they still rely on `ResourceQuota` underneath, so they inherit its single-counter per-type gap and admission-time enforcement. Where an org runs HNC/Capsule, A's per-namespace caps integrate with the inherited quotas (read the effective `ResourceQuota` per namespace); mandating a tenancy model on orgs that don't want one is out of scope.

### F. Custom validating admission webhook

A separate webhook that observes scaling-related operations (VA status patches, Deployment replica updates) and rejects ones that would exceed quota.

| Strengths | Weaknesses |
|---|---|
| K8s-idiomatic — admission is *the* extension point for "should this be allowed?" | New deployable, new TLS cert chain, new failure mode (webhook unavailable ⇒ scaling broken). |
| Decouples policy from scaler. | Information asymmetry — the webhook has to re-discover quota state per-request. Slow path. |
| Easy to layer multiple policies. | Hard to give operators a coherent error: WVA's logs say "scaled to N", webhook says "rejected", state diverges. |

**Verdict:** Higher operational complexity than embedding the check in WVA, with no clear advantage when WVA is the single source of scaling decisions.

### G. GPU operator-level partitioning (NVIDIA MIG, time-slicing)

**Orthogonal — not a substitute, omitted from the matrix.** MIG/time-slicing define *what counts as one GPU* (physical/temporal partitioning); they do not express *who may use how many*. They compose with A (A allocates whatever the node advertises as a GPU) but cannot answer "team-a ≤ 8× H100." Listed only to disclaim the common conflation of partitioning with quota.

### H. HPA / KEDA `maxReplicas`

Use the existing autoscaler's `maxReplicas` as a per-variant cap.

| Strengths | Weaknesses |
|---|---|
| Already in the stack, zero new dependencies. | **Per-variant only**, not per-GPU-type. Operator has to compute the cap manually for each VA. |
| Familiar. | No cross-variant budget sharing ("team-a's H100 budget = 8 across all their variants"). |

**Verdict:** Necessary lower-bound (every VA already has it), but not a substitute for type-level quota.

## 4. Comparison matrix

| Property | A. WVA `QuotaLimiter` | A2. `ResourceQuotaReader` | B. Classic `ResourceQuota` alone | B2. DRA (K8s 1.34+) | C. OpenShift CRQ | D. Kueue | H. HPA `maxReplicas` |
|---|---|---|---|---|---|---|---|
| Per-GPU-type granularity | ✅ | ❌ | ❌ | ✅ (per-DeviceClass) | ❌ | ✅ (per-`ResourceFlavor`) | ❌ |
| Cluster scope | ✅ | ❌ | ❌ | ❌ (namespace) | ✅ | ✅ (`ClusterQueue`) | ❌ |
| Namespace scope | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ (`LocalQueue`) | ❌ |
| Enforced at decision time (not pod admission) | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ (admission) | ✅ |
| Works on any conformant K8s | ✅ | ✅ | ✅ | ❌ (needs ≥1.34 + driver) | ❌ (OCP-only) | ✅ | ✅ |
| Zero new operators / CRDs | ✅ | ✅ | ✅ | ❌ (DRA driver) | ✅ | ❌ | ✅ |
| Surfaces in WVA decision trace | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ |
| K8s-native operator UX | ❌ (new YAML) | ✅ | ✅ | ✅ | ✅ (OCP) | partial | ✅ |
| Cross-variant budget sharing | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| Exclude/bypass list | ✅ | ❌ | ❌ | ❌ | ❌ | partial (priority classes) | n/a |
| Quota enforcement maturity | prototype | not yet | mature | basic in 1.34; #4840 WIP | mature | mature | mature |

(Option F, a custom admission webhook, is omitted: a webhook is arbitrary admission logic whose capability depends entirely on implementation, so it has no fixed profile to tabulate — see §3.F for why it is rejected on operational grounds. E and G are orthogonal mechanisms, also omitted.)

## 5. Recommendation

**Adopt option A (WVA-internal `QuotaLimiter`) as the simple, universal baseline, and the K8s-native *readers* — A2 (`ResourceQuotaReader`) now, a `KueueReader`/DRA reader later — as additive reuse layered on top.** A resolves #1002 on any cluster with zero runtime dependencies (no new operators, CRDs, or webhooks); A2 ships as its own smaller fast-follow PR and reuses an existing `ResourceQuota` where the admin already declares one.

### Why A is the resolution of #1002

#### Per-criterion breakdown

#1002 defines **four enumerated acceptance criteria**: (1) per-GPU-type cap at **cluster** scope; (2) per-GPU-type cap at **namespace** scope; (3) cluster **+** namespace **coexistence**; (4) a **bypass/exclude** list for system tenants. Two further **architectural requirements** decide the outcome; they are scored in a separate block below — not to silently expand #1002's list, but because:

- **Requirement 5, decision-time enforcement, is not invented — it is #1002's own framing clause made measurable.** #1002 asks for caps *"enforced by the WVA scaling pipeline"* (§1); a cap enforced at *pod admission* (by the kube-apiserver) is by definition **not** enforced by the WVA pipeline. The anticipated-capacity trap (§3.D) is what that distinction costs in practice.
- **Requirement 6, works on any conformant K8s,** is the portability bar this project holds for every feature (no OS-vendor or K8s-version binding).

The grid below re-projects the decision-relevant rows of the §4 matrix into #1002's acceptance framing — §4 is the full capability comparison; this is the *verdict against the criteria*. It scores every concrete option against both groups (✅ satisfies, ~ partial, ❌ fails):

| Requirement | A | A2 | B | B2 | C | D | H |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| *— #1002 acceptance criteria —* | | | | | | | |
| 1. Per-GPU-type @ **cluster** scope | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ |
| 2. Per-GPU-type @ **namespace** scope | ✅ | ❌ | ❌ | ✅ | ❌ | ✅ | ❌ |
| 3. Cluster **+** namespace coexistence | ✅ | ❌ | ❌ | ❌ | ~ | ✅ | ❌ |
| 4. Exclude/bypass for system tenants | ✅ | ❌ | ❌ | ❌ | ❌ | ~ | n/a |
| *— WVA pipeline requirements (not in #1002) —* | | | | | | | |
| 5. Decision-time enforcement (not admission) | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ |
| 6. Works on any conformant K8s | ✅ | ✅ | ✅ | ❌ | ❌ | ✅ | ✅ |

**A is the only column with ✅ in every row.** Reading the failures off the grid:

- **A2 (`ResourceQuotaReader`) / B (`ResourceQuota` alone)** — a single `requests.nvidia.com/gpu` counter, namespace-only: no per-GPU-type (1, 2), no cluster scope (1, 3), no exclude (4). A2 cannot express `{H100: 8, A100: 4}` in one namespace, so the heterogeneous case (#1018) is out of reach. But A2 *does* clear both WVA requirements (5, 6) — which is exactly why it is a useful **reader** for the homogeneous / single-counter persona, not a dead end.
- **B2 (DRA)** — per-`DeviceClass` quota gives per-type at the namespace level (2 ✅), but `ResourceQuota` is namespace-only, so no cluster aggregate (1, 3 ❌) and no exclude (4 ❌); and it fails *both* WVA requirements — admission-time enforcement (5 ❌) and K8s ≥ 1.34 only (6 ❌). [KEP #4840](https://github.com/kubernetes/enhancements/issues/4840) is still WIP.
- **C (OpenShift `ClusterResourceQuota`)** — cluster aggregate (3 ~, paired with `ResourceQuota`) but single-counter, so no per-type (1, 2 ❌), no exclude (4 ❌); admission-time (5 ❌), OCP-only (6 ❌).
- **D (Kueue)** — **the closest of any alternative**: it meets criteria 1–3 (`ClusterQueue`/`LocalQueue` + `ResourceFlavor`) and approximates 4. It is **requirement 5** that disqualifies it as the scaler — it enforces at admission, triggering the anticipated-capacity trap (§3.D). The fit is therefore a `KueueReader` `ConstraintProvider` (sibling of A2) that reads its quota into WVA's decision loop, not Kueue as the scaler.
- **H (HPA `maxReplicas`)** — a per-variant cap, not per-type: fails 1–3; exclude n/a. It clears 5 and 6 but cannot express type-level budget.
- **F (admission webhook)**, **E (HNC/Capsule)**, **G (MIG/time-slicing)** — omitted from the grid: F is arbitrary admission logic with no fixed profile and fails requirement 5; E/G are orthogonal (tenancy / physical partitioning). See §3.

In short: D is the lone alternative that clears criteria 1–3 (and approximates 4), and its only outright ❌ is **requirement 5** (the trap); A2 clears both requirements but not the per-type criteria (a future `KueueReader`/DRA reader clears those too, but ships later). A clears every row — which is the whole grid, not an assertion.

Beyond the grid, three points carry the recommendation:

1. **Decision-time enforcement (row 5), in depth.** WVA never asks for more replicas than allowed, so operators see capped scaling decisions in the controller's trace rather than pods stuck at admission. Admission-time enforcement is *not* equivalent: classic `ResourceQuota` and DRA **reject** the over-quota pod at creation (the rollout stalls below desired, WVA stays blind to the cap), while Kueue **gates** it `Pending` and triggers the anticipated-capacity trap (mechanics in §3.D). Reading the cap at decision time avoids both — and this property is shared by A *and* the readers (A2, a future `KueueReader`); the load-bearing requirement is that the cap binds in WVA's decision loop, wherever it is sourced, not at pod admission.
2. **`DecisionStep` observability** (not scored in the grid) — the limiter emits `limited by quota[scope=…, type=…]` directly into WVA's per-cycle trace, fitting the observability work in #913 / #915.
3. **Composable** (not scored in the grid) — A implements the existing `Inventory` interface, so the limiter chain (#1003) composes it with `TypeInventory` and any K8s-native readers (`ResourceQuotaReader` A2, a future `KueueReader`, later DRA) via `min(physical, wva-quota, k8s-native-quota)` with no schema change. There is no "conflict" between sources: the tightest cap binds, and the `DecisionStep` names which one (`limited by quota[…]` vs `limited by resourceQuota[…]`).

The design's shape is **start simple and universal, then layer reuse.** A assumes no particular quota system — it works on any conformant cluster, day one, with zero dependencies. The readers then add ecosystem-specific value: where GPUs are already governed by `ResourceQuota` (A2), Kueue (`KueueReader`), or DRA, WVA reads that source through the same chain (`min(physical, …)`) so the operator never redeclares the cap. A is the foundation everyone gets; the readers are progressive enhancements for whatever quota system you already run.

### DecisionStep trace examples

The cap surfaces in WVA's per-cycle trace for every binding scenario:

- **Quota binds** (`physical=24`, `quota[H100, ns:team-a]=8`, `desired=12`): `min → 8` → `DecisionStep{ limited by quota[scope=ns:team-a, type=H100], desired=12, capped=8 }`.
- **Physical inventory binds** (`physical=6`, `quota=8`, `desired=12`): `min → 6` → `DecisionStep{ limited by inventory[type=H100], desired=12, capped=6 }`.
- **Both bind** (`physical=10`, `quota=8`): `min → 8` → the trace names the *binding* source (`quota`) and records `inventory[H100]=10` as the looser bound.

### Why A2 ships alongside A

The platform-admin persona — administrators who already govern the cluster with `kind: ResourceQuota` and expect WVA to honor what's already declared — is a real, separate need that A does not address. The load-bearing properties for A2 are:

1. **Zero new operator-facing schema.** Admins keep using the K8s primitive they already know; WVA picks up `ResourceQuota` objects via an informer.
2. **Single source of truth.** When `ResourceQuota` already exists in a namespace, A2 reads it directly — no duplicate WVA-specific config drifts from the K8s-native one.
3. **Decision-time enforcement, like A.** A2 reads the policy from K8s but applies it inside the WVA decision loop, so the same `LimitedBy` trace and stuck-rollout-prevention properties apply. This is what distinguishes A2 from option B.

A2 reuses the same `ConstraintProvider` plumbing A introduces, so the two compose via the limiter chain (#1003) as `min(physical, wva-quota, resource-quota)` without redesign.

### How A and A2 compose (different granularities, same enforcement layer)

A and A2 express caps at **different granularities** — A is per-GPU-type (`{H100: 8, A100: 4}`), A2 is a single per-extended-resource counter (`requests.nvidia.com/gpu: 10`). They are not in conflict, because they constrain different things and **both bind at the same point** (WVA's decision loop), so the chain just applies the tightest applicable bound:

- A2's counter is an **aggregate ceiling** on the sum across all types in a namespace: `Σ_type alloc[type, ns] ≤ A2[ns]`.
- A's caps are **per-type ceilings**: `alloc[type, ns] ≤ A[type, ns]`.
- Both hold simultaneously: for each type the chain takes `min(physical[type], A[type, ns])`, and the per-namespace sum is *additionally* clamped by `A2[ns]`. The `DecisionStep` names whichever bound actually capped the decision (`limited by quota[type=H100]` vs `limited by resourceQuota[ns=…]`).

Concretely: a namespace with `A = {H100: 8, A100: 4}` and a pre-existing `ResourceQuota` of `requests.nvidia.com/gpu: 10` (read via A2) is held to **8 H100 plus at most 2 A100** — once 8 H100 are allocated, only 2 of the 10-GPU aggregate remain. The per-type cap and the aggregate ceiling each do work the other cannot, so they coexist cleanly; an operator may also run just one. (The aggregate actually available to WVA is `ResourceQuota.hard` *minus* GPUs already consumed by non-WVA workloads in the namespace; reconciling `status.used` against WVA's own per-cycle usage is the A2 design question called out in the scope split — the `10` here is the simplifying case where WVA is the only consumer.)

**The reason they compose is the enforcement layer, not the granularity.** A and A2 both enforce at *decision time*, so both can be a term in the same `min()`. Option B carries the *same single-counter granularity as A2* yet does **not** compose this way, because it binds at *admission* — WVA never sees it in the chain, and the two enforcement points diverge (WVA scales, the API server rejects). That admission-vs-decision distinction — even though #1002 did not list it as an explicit acceptance criterion — is the property that actually determines whether a cap can join WVA's decision loop at all. It is the dividing line in the §4 matrix row "enforced at decision time," and the failure it prevents is the anticipated-capacity trap (§3.D).

### Scope split

| Feature | Issue | Implementation status | Closes |
|---|---|---|---|
| **A — `QuotaLimiter`** | #1002 (this proposal) | Prototype (see §3.A) | All four acceptance criteria of #1002 |
| **A2 — `ResourceQuotaReader`** | New issue, to be filed alongside merging A | Not yet started; estimated ~300 lines (informer + `ConstraintProvider`) | Platform-admin-persona need; not part of #1002 |
| **`KueueReader`** (future) | New issue, where Kueue is deployed | Not started; a `ConstraintProvider` reading `ResourceFlavor`/`ClusterQueue` nominal quota | Reuses existing Kueue per-GPU-type quota; same chain as A2 |

A2 is intentionally a separate, smaller PR — it has its own design questions (informer setup, handling of `ResourceQuota.status.used` vs. WVA's per-cycle computed usage, eventual consistency across the watch) that deserve their own design discussion rather than being bundled into #1002.

## 6. Configuration surface and deployment scope

§3–5 settle *where the cap is enforced* (the WVA decision loop) and *what mechanism* enforces it. Two further questions — where the operator *declares* quota, and how that interacts with WVA's deployment scope — are settled jointly with the config-UX proposal (#1194), which owns WVA's user-facing configuration surface.

### 6.1 Where the operator declares quota

Option A's prototype reads the cap from a ConfigMap, but that is **not** the proposed operator surface. The config-UX proposal consolidates all WVA *policy* onto a single typed `ScalingPolicy` CRD after the VA CRD deprecation, specifically to avoid the configuration fragmentation that results from each feature adding its own ConfigMap. Quota is a scaling *constraint* — the GPU budget the optimizer allocates within — so it belongs on that single surface.

**Recommendation: quota is declared directly on the cluster-default `ScalingPolicy` (`spec.quota`), with no interim standalone quota ConfigMap.** The `QuotaLimiter` mechanism (§3.A) reads its caps from that object; the prototype's ConfigMap stays an internal/test detail, never a shipped operator surface. Shipping a quota ConfigMap first would introduce exactly the fragmentation #1194 exists to remove and that we would immediately deprecate — so quota lands on `ScalingPolicy` directly. This keeps the single-surface rule: quota is authored in exactly one place, owned by the cluster admin. The cost is sequencing — quota's operator surface is gated on the cluster-default `ScalingPolicy` from #1194 landing; the enforcement mechanism (§3.A) can be built and tested independently against the prototype ConfigMap in the meantime.

**On the "new schema" objection.** `spec.quota` is a new *field*, but not the kind of surface this project pushes back on elsewhere: it is one typed, validated field on the *single* `ScalingPolicy` surface — not a disconnected env var, a hand-maintained label convention, or a separate ConfigMap, and it composes rather than replacing node discovery. It is also the *irreducible* cost of per-GPU-type: no native K8s object expresses `{H100: 8, A100: 4}` on its own (the single-counter `ResourceQuota`; per-`DeviceClass` quota needs DRA on 1.34+). Where a native per-type source *does* exist — DRA's per-`DeviceClass` quota or Kueue's `ResourceFlavor` — WVA reads it through a `ConstraintProvider` and adds **no** schema at all (§5). So `spec.quota` is the minimum config for the case nothing native covers, not a parallel system; the zero-schema options are also zero-per-type until a native per-type source is present.

### 6.2 Deployment scope: quota is cluster-scoped

Quota enforcement is intrinsically **cluster-scoped**, which constrains how WVA may be deployed when quota is in use:

- **A cluster aggregate needs a cluster-wide view.** A namespace-scoped WVA instance (`--watch-namespace`) sees only its own namespace, so it can enforce only that namespace's cap — not an overall cluster cap. Running one WVA per namespace cannot honor a cluster aggregate: the sum of per-namespace caps can exceed physical cluster capacity, and independent instances each see the full node inventory without coordinating, so they would collectively overcommit.
- **Ownership.** Per-namespace caps are a cluster-admin decision; a tenant must not be able to raise their own. A per-namespace deployment would place the quota config in the tenant's namespace/hands.

So cluster-scoped quota (the cluster-scope half of #1002) requires a cluster-scoped WVA controller and a cluster-admin-owned surface. A namespace-scoped WVA can still enforce a namespace-local cap it is given, but cannot own or arbitrate the cluster aggregate. This matches the persona split in #1194: thresholds/scale-to-zero are tenant-owned and namespace-local; quota is cluster-admin-owned and cluster-scoped.

### 6.3 Worked examples

The four declaration shapes and the resulting behavior. The YAML below shows an **illustrative** shape to make behavior concrete — the normative schema is owned by #1194 (§6.1), and `spec.quota` and the prototype ConfigMap are shape-equivalent.

**Cluster-only.** One cluster aggregate, no per-namespace caps:

```yaml
spec:
  quota:
    cluster:
      H100: 16          # WVA's total H100 across all namespaces ≤ 16
      A100: -1          # unlimited
```
Behavior: every namespace draws from the shared 16× H100; the optimizer's cluster-wide H100 allocation across all VAs is capped at 16. No per-tenant isolation.

**Namespace-only.** Independent per-namespace caps, no cluster aggregate:

```yaml
spec:
  quota:
    namespaces:
      team-a: { H100: 8 }
      team-b: { H100: 4 }
```
Behavior: `team-a ≤ 8`, `team-b ≤ 4`, enforced independently. The cluster total is bounded only by physical capacity (`min(physical, per-ns cap)` per namespace), so the sum of namespace caps *may* exceed physical — physical inventory then binds and the `DecisionStep` says `limited by inventory`.

**Cluster + namespace simultaneously.** Per-tenant caps *under* a cluster ceiling:

```yaml
spec:
  quota:
    cluster:    { H100: 16 }
    namespaces:
      team-a:   { H100: 8 }
      team-b:   { H100: 8 }
```
Behavior: each namespace is held to its own cap (8 each) **and** the sum across namespaces is clamped to the cluster ceiling (16). Here `8 + 8 = 16`, so both can run full; if a third namespace also wanted H100 it would get 0 (the cluster aggregate is exhausted), and its trace reads `limited by quota[scope=cluster, type=H100]`.

**Namespace cap exceeds the cluster cap.** `min` always binds — the looser cap can never raise the tighter:

```yaml
spec:
  quota:
    cluster:    { H100: 16 }
    namespaces:
      team-a:   { H100: 20 }   # > cluster
```
Behavior: `team-a` is effectively held to **16** (the cluster aggregate), not 20 — `min(physical, ns-cap, remaining cluster aggregate)`. The namespace cap above the cluster cap is silently clamped (the cluster ceiling is the outer bound), and admission of the `ScalingPolicy` emits a **validation warning** that `team-a.H100 (20) > cluster.H100 (16)` so the operator knows the namespace cap is dead weight above the ceiling.

**When namespace caps oversubscribe the cluster cap.** If `Σ namespace caps > cluster cap` and demand is high enough to hit the ceiling, the quota limiter still enforces both bounds (`min(ns-cap, remaining cluster aggregate)`), but it does **not** decide *which* namespace wins the contested GPUs. That is the optimizer's existing allocation order; priority-weighted redistribution under contention is the concern of the separate rescale proposal (it *consumes* this budget, it does not *set* it). The examples above use non-oversubscribed caps to isolate the quota mechanic from the contention policy.

