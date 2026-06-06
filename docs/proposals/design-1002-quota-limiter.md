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
- Enforce the cap at scaling-decision time so WVA never *requests* more replicas than allowed.
- Compose cleanly with the existing `TypeInventory` so physical capacity remains an upper bound (sub-issue #1003). `TypeInventory` is re-read from node discovery each cycle, so `min(physical, quota)` tracks node add/drain automatically — the quota only ever lowers the live physical bound.
- Support an explicit "unlimited" sentinel and an "exclude this namespace" affordance for system tenants. An excluded namespace is *not capped*, but its GPU usage still counts against physical capacity (exclusion removes the cap, not the accounting), so the cluster aggregate cannot overcommit.
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

**Verdict:** The right long-term direction for K8s-native per-GPU-type quotas, but not a substitute for #1002 in the WVA decision pipeline today. WVA should plan to integrate with DRA in a future phase (read DeviceClass-scoped quotas as additional chain inputs, alongside any `ResourceQuotaReader`).

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

**Verdict:** Not the enforcement mechanism for #1002, but the right *source* to read where it is deployed. Because Kueue enforces at admission, using it alone produces the **anticipated-capacity trap**: replicas WVA requested beyond quota sit `Pending` (gated by Kueue), WVA folds them into `anticipatedSupply` (`saturation_v2/analyzer.go` counts `ready + pending` as incoming capacity), so `requiredCapacity` is suppressed and the optimizer won't scale a sibling variant on a different `ResourceFlavor` that could serve the same `InferencePool` — the pool stays under-served while WVA believes it is converging. The fix is decision-time enforcement: WVA *reads* Kueue's `ResourceFlavor`/`ClusterQueue` quota as a `ConstraintProvider` (the same reader family as A2's `ResourceQuotaReader`) and caps the request, so it never asks for replicas Kueue would gate. Where an org already runs Kueue, that reader reuses its per-GPU-type quota directly; where it does not, Option A provides the same cap with no new operator.

### E. Hierarchical Namespaces (HNC) / Capsule

Multi-tenancy frameworks that nest namespaces or wrap them in a `Tenant` CRD with inherited quotas.

| Strengths | Weaknesses |
|---|---|
| Solves multi-tenancy at the platform layer, not just for GPUs. | Heavy: add a tenancy operator, retrain operators on a new namespace model. |
| Inheritance is a clean mental model for nested teams. | Still uses `ResourceQuota` underneath — inherits its per-GPU-type limitation. |
| Useful when an org already runs HNC/Capsule. | Forces a tenancy model on orgs that don't want one. |

**Verdict:** Orthogonal. If an org already runs HNC, our `QuotaLimiter` should integrate cleanly with it (read effective `ResourceQuota` per namespace). But mandating it is wrong.

### F. Custom validating admission webhook

A separate webhook that observes scaling-related operations (VA status patches, Deployment replica updates) and rejects ones that would exceed quota.

| Strengths | Weaknesses |
|---|---|
| K8s-idiomatic — admission is *the* extension point for "should this be allowed?" | New deployable, new TLS cert chain, new failure mode (webhook unavailable ⇒ scaling broken). |
| Decouples policy from scaler. | Information asymmetry — the webhook has to re-discover quota state per-request. Slow path. |
| Easy to layer multiple policies. | Hard to give operators a coherent error: WVA's logs say "scaled to N", webhook says "rejected", state diverges. |

**Verdict:** Higher operational complexity than embedding the check in WVA, with no clear advantage when WVA is the single source of scaling decisions.

### G. GPU operator-level partitioning (NVIDIA MIG, time-slicing)

Configure NVIDIA's GPU Operator to expose MIG slices or time-shared GPUs as multiple "virtual GPUs."

| Strengths | Weaknesses |
|---|---|
| Physical partitioning — the most defensible cap there is. | Granularity is per-physical-GPU (MIG profile choice), not per-tenant. |
| Composes with all the above (just changes what "1 GPU" means). | Doesn't help with "team-a may use 8 H100s." That's still a policy decision on top. |

**Verdict:** Orthogonal. MIG defines what a "GPU" is; `QuotaLimiter` allocates GPUs. Both can coexist.

### H. HPA / KEDA `maxReplicas`

Use the existing autoscaler's `maxReplicas` as a per-variant cap.

| Strengths | Weaknesses |
|---|---|
| Already in the stack, zero new dependencies. | **Per-variant only**, not per-GPU-type. Operator has to compute the cap manually for each VA. |
| Familiar. | No cross-variant budget sharing ("team-a's H100 budget = 8 across all their variants"). |

**Verdict:** Necessary lower-bound (every VA already has it), but not a substitute for type-level quota.

## 4. Comparison matrix

| Property | A. WVA `QuotaLimiter` | A2. `ResourceQuotaReader` | B. Classic `ResourceQuota` alone | B2. DRA (K8s 1.34+) | C. OpenShift CRQ | D. Kueue | F. Webhook | H. HPA `maxReplicas` |
|---|---|---|---|---|---|---|---|---|
| Per-GPU-type granularity | ✅ | ❌ | ❌ | ✅ (per-DeviceClass) | ❌ | ✅ (per-`ResourceFlavor`) | depends | ❌ |
| Cluster scope | ✅ | ❌ | ❌ | ❌ (namespace) | ✅ | ✅ (`ClusterQueue`) | depends | ❌ |
| Namespace scope | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ (`LocalQueue`) | depends | ❌ |
| Enforced at decision time (not pod admission) | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ (admission) | depends | ✅ |
| Works on any conformant K8s | ✅ | ✅ | ✅ | ❌ (needs ≥1.34 + driver) | ❌ (OCP-only) | ✅ | ✅ | ✅ |
| Zero new operators / CRDs | ✅ | ✅ | ✅ | ❌ (DRA driver) | ✅ | ❌ | ❌ | ✅ |
| Surfaces in WVA decision trace | ✅ | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ |
| K8s-native operator UX | ❌ (new YAML) | ✅ | ✅ | ✅ | ✅ (OCP) | partial | n/a | ✅ |
| Cross-variant budget sharing | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | depends | ❌ |
| Exclude/bypass list | ✅ | ❌ | ❌ | ❌ | ❌ | partial (priority classes) | depends | n/a |
| Quota enforcement maturity | prototype | not yet | mature | basic in 1.34; #4840 WIP | mature | mature | varies | mature |

## 5. Recommendation

**Adopt option A (WVA-internal `QuotaLimiter`) as the simple, universal baseline, and the K8s-native *readers* — A2 (`ResourceQuotaReader`) now, a `KueueReader`/DRA reader later — as additive reuse layered on top.** A resolves #1002 on any cluster with zero dependencies; A2 ships as its own smaller fast-follow PR and reuses an existing `ResourceQuota` where the admin already declares one.

### Why A is the resolution of #1002

A is the only candidate in §3 that meets #1002 on **every dimension that matters** — per-GPU-type granularity, cluster + namespace scope and their coexistence, a bypass-list for system tenants, *and* the load-bearing properties detailed below. Each other option fails at least one:

- **A2 (`ResourceQuotaReader`)** is namespace-only and per-extended-resource-only — it inherits the single-counter limitation of `requests.nvidia.com/gpu`, so it cannot express `{H100: 8, A100: 4}` in a single namespace. The heterogeneous-cluster case (#1018) is out of reach. A2 covers a different operator persona; it does not close #1002.
- **B (classic `ResourceQuota` alone)** has the same per-GPU-model gap as A2 plus admission-time enforcement, which leaves WVA blind to the cap and produces stuck rollouts instead of clean `LimitedBy` traces.
- **B2 (DRA)** closes the per-GPU-model gap via per-`DeviceClass` quotas, but is GA only on K8s ≥ 1.34, enforces at admission time, and has unresolved quota mechanics ([KEP #4840](https://github.com/kubernetes/enhancements/issues/4840)).
- **C (OpenShift `ClusterResourceQuota`)** adds cluster scope but is OCP-only and inherits the per-GPU-model gap.
- **D (Kueue)** has the right granularity via `ResourceFlavor`, but enforces at *admission* — used alone it triggers the anticipated-capacity trap (§3.D). The fit is a `KueueReader` `ConstraintProvider` (sibling of A2) that reads its quota into the chain, not Kueue as the scaler.
- **F (admission webhook)** adds operational complexity without a clear advantage over an in-process limiter.
- **E (HNC/Capsule)**, **G (MIG/time-slicing)**, and **H (HPA `maxReplicas`)** are orthogonal — they compose with A rather than substituting for it.

The load-bearing properties that make A the right primary mechanism for #1002 are:

1. **Per-GPU-type granularity** on any conformant K8s version, without an OS-vendor binding or a K8s-version floor.
2. **Decision-time enforcement** — WVA never asks for more replicas than allowed, so operators see capped scaling decisions in the controller's trace rather than pods stuck at admission. Admission-time enforcement is *not* equivalent, and it fails in one of two ways depending on the mechanism. Classic `ResourceQuota` and DRA **reject** the over-quota pod at creation, so the rollout stalls below desired and WVA stays blind to the cap. Kueue instead **gates** the pods — created, then held `Pending` by a scheduling gate — and WVA folds those held pods into `anticipatedSupply` (it counts `ready + pending` as incoming capacity), so `requiredCapacity` is suppressed and a sibling variant that could serve the same `InferencePool` is never scaled: the **anticipated-capacity trap** (§3.D). Reading the cap at decision time avoids both — and this property is shared by A *and* the readers (A2, a future `KueueReader`); the load-bearing requirement is that the cap binds in WVA's decision loop, wherever it is sourced, not at pod admission.
3. **`DecisionStep` observability** — the limiter emits `limited by quota[scope=…, type=…]` directly into WVA's per-cycle trace, fitting the observability work in #913 / #915.
4. **Composable** — A implements the existing `Inventory` interface, so the limiter chain (#1003) composes it with `TypeInventory` and any K8s-native readers (`ResourceQuotaReader` A2, a future `KueueReader`, later DRA) via `min(physical, wva-quota, k8s-native-quota)` with no schema change. There is no "conflict" between sources: the tightest cap binds, and the `DecisionStep` names which one (`limited by quota[…]` vs `limited by resourceQuota[…]`).

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

### Scope split

| Feature | Issue | Implementation status | Closes |
|---|---|---|---|
| **A — `QuotaLimiter`** | #1002 (this proposal) | Prototype on `feat/quota-limiter`; 8 commits, 50+ Ginkgo specs | All four acceptance bullets of #1002 |
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

