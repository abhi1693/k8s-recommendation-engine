# Planned Change: Standalone Predictive Kubernetes Workload Optimizer

Status: Planned

## Summary

Transform the current periodic Shipyard recommender into a Kubernetes-native,
GitOps-only optimizer for existing cluster capacity. Karpenter is strictly an
engineering-quality reference; there will be no Karpenter integration and no
node provisioning or removal.

The optimizer will explicitly manage Deployments, StatefulSets, DaemonSets,
Jobs, and CronJobs. It will forecast demand before spikes, simulate whether
proposed pods fit the existing cluster, minimize waste within SLO and disruption
constraints, and deliver validated changes through GitHub pull requests or
direct branch commits.

The current implementation provides a useful prototype: Prometheus percentiles,
CPU, memory, and replica recommendations, SQLite history, stability gates, Fleet
manifest edits, and rollback. It remains a periodic single-process heuristic and
lacks informer-driven reconciliation, CRDs, cluster-wide scheduling simulation,
probabilistic forecasting, high availability, service observability, and safe
rollout orchestration.

Karpenter-level maturity means adopting comparable engineering patterns such as
typed policy resources, event-driven controllers, scheduling simulation, status
conditions, disruption budgets, high availability, metrics, and rigorous scale
testing. It does not mean copying Karpenter's node-management responsibilities.

## Product Boundaries and Defaults

- The optimizer never creates, deletes, or resizes nodes and does not integrate
  with Karpenter.
- Workloads are explicitly enrolled through Kubernetes CRDs.
- The supported workload kinds are Deployments, StatefulSets, DaemonSets, Jobs,
  and CronJobs. Raw Pods and ReplicaSets are observed but never owned.
- GitOps is the only actuation path. The optimizer never directly patches
  application controllers or Pods.
- GitHub is the only hosted Git provider in the first production release. Local
  dry-run remains available.
- Raw YAML, Kustomize, and Helm values are supported as Git sources.
- SLO safety overrides efficiency. Missing, stale, or uncertain evidence causes
  the optimizer to hold current capacity.
- Prometheus provides historical and current workload signals; Kubernetes APIs
  provide topology, ownership, health, and capacity truth.
- Production forecasting runs as a separate Python service. The Go controller
  retains a conservative deterministic fallback.
- Version-one performance acceptance targets clusters with up to 1,000 nodes and
  50,000 pods.

## Target Architecture

### Kubernetes control plane

- Replace the timer loop with a Go `controller-runtime` manager using shared
  informers, indexed caches, rate-limited work queues, exponential backoff,
  leader election, periodic resynchronization, and event batching.
- Run multiple controller replicas for high availability. Only the elected
  leader creates Git changes; non-leaders keep caches warm for fast failover.
- Reconcile relevant changes to workload controllers, Pods, Nodes, autoscalers,
  quotas, LimitRanges, PDBs, Services, EndpointSlices, PVCs, PVs, profiles,
  recommendations, and Git convergence state.
- Build immutable, versioned cluster snapshots instead of repeatedly listing
  resources for every workload.
- Keep Kubernetes permissions read-only for application resources. Writes are
  restricted to optimizer CRDs, Events, status subresources, and leader-election
  Leases.
- Expose liveness, readiness, Prometheus metrics, optional authenticated
  profiling, structured logs, OpenTelemetry traces, build metadata, and
  controller health metrics.

### Public CRDs

Introduce conversion-ready `optimization.io/v1alpha1` APIs:

- `OptimizationProfile` is namespaced and contains the explicit workload list,
  policy defaults, Git repository reference, SLOs, mutable fields, resource and
  replica bounds, forecast signals, planned-event calendars, disruption budgets,
  rollout windows, and controller-specific options.
- `GitRepository` defines the GitHub App credential Secret reference, repository,
  base branch, allowed paths, source format, delivery mode, commit branch,
  auto-merge rules, and GitOps convergence timeout.
- `WorkloadRecommendation` is created per target and contains the snapshot hash,
  forecast horizons and quantiles, current and proposed resources, feasibility,
  confidence, typed evidence, validation results, Git commit or pull request,
  convergence state, and observed outcome.
- Standard status conditions include `Observed`, `DataReady`, `ForecastReady`,
  `Feasible`, `SLOSafe`, `Stable`, `Proposed`, `Merged`, `Converged`, `Healthy`,
  `RolledBack`, and `Degraded`. Every condition includes `observedGeneration`, a
  reason, a message, and a transition time.
- CRD status stores compact decision metadata only. Raw time-series data remains
  in Prometheus.

Each enrolled workload must provide an explicit source mapping:

- Raw YAML: manifest path and resource identity.
- Kustomize: overlay root, resource identity, and a dedicated generated optimizer
  patch.
- Helm: values file and explicit value paths for replicas, per-container
  resources, autoscaler settings, Job parallelism, and CronJob template
  resources. The optimizer never guesses Helm value keys.

### Forecasting service

Create a separate Python service with a versioned gRPC API:

- `Train` accepts normalized time-series references, feature configuration,
  evaluation windows, and workload identity.
- `Forecast` returns p50, p90, and p99 predictions over requested horizons,
  uncertainty, model version, feature freshness, change-point state, and fallback
  status.
- `Backtest` returns rolling-origin pinball loss, MASE, bias, interval coverage,
  and comparison with naive baselines.
- `Health` and model-registry endpoints expose readiness and the training backlog.

The forecasting pipeline will:

- Use quantile gradient-boosted trees as the primary model, with lag, rolling,
  calendar, trend, deployment, and external leading-signal features.
- Ensemble the primary model with seasonal-naive and ETS forecasts, dynamically
  weighted by recent rolling backtest loss.
- Detect trend breaks and anomalies separately so exceptional traffic is not
  immediately learned as normal seasonality.
- Produce multiple forecast horizons based on measured Git delivery latency,
  GitOps synchronization, rollout duration, pod startup, and a safety margin.
- Fall back to conservative seasonal and quantile forecasts whenever training,
  inference, data freshness, or uncertainty checks fail.
- Keep Prometheus as the raw-data store. Production deployments use PostgreSQL
  for compact model metadata and an S3-compatible store for artifacts; SQLite and
  a PVC remain development-only options.
- Bound training concurrency and prioritize models approaching their required
  action horizon.

### Cluster-wide optimizer and scheduler simulation

The optimizer will account for:

- Node allocatable resources, current requests, actual usage, system and
  DaemonSet overhead, init containers, sidecars, pod overhead, ephemeral storage,
  extended resources, and maximum pod counts.
- ResourceQuotas, LimitRanges, PriorityClasses, taints and tolerations, node
  selectors and affinity, pod affinity and anti-affinity, topology spread, PV
  topology, and unhealthy or unavailable nodes.
- Existing HPA, VPA, KEDA, custom scale-subresource owners, PDBs, rollout state,
  and Git field ownership.

For every decision it will:

1. Generate candidate replica and per-container request combinations from
   forecast quantiles and policy bounds.
2. Simulate every candidate against existing nodes and scheduling constraints.
3. Reject impossible changes and report `CapacityInsufficient` with the exact
   exhausted resource or incompatible constraint.
4. Select candidates lexicographically: schedulability and hard safety first,
   SLO and error-budget compliance second, disruption minimization third, and
   wasted requested capacity last.
5. Reserve configurable cluster headroom and failure-domain capacity so a
   recommendation remains viable after a node or zone failure where topology
   permits it.
6. Produce deterministic output for an identical cluster snapshot, profile,
   source revision, and model version.

## Implementation Roadmap

### Phase 1: Production foundation

- Split the monolithic analyzer into observer, forecast client, optimizer,
  scheduler simulator, policy, actuator, convergence, and reporting components
  behind narrow interfaces.
- Generate CRDs, validation and defaulting webhooks, status patching, leader
  election, work queues, health endpoints, controller metrics, feature gates, and
  graceful shutdown.
- Preserve `analyze` as an offline troubleshooting command while making the
  controller and CRDs authoritative.
- Provide migration from the current `ApplicationProfile` file and deprecate the
  file-based API after a documented compatibility period.
- Replace string-parsed reason codes with typed evidence and decision records;
  human-readable reason codes remain rendered output only.
- Add schema migrations, retention, and compaction for decision and model state.

### Phase 2: Observation and field ownership

- Build informer-backed indexes for controller-to-pod, service-to-workload,
  PDB-to-workload, autoscaler-to-target, PVC/PV topology, and node capacity.
- Detect Git drift, paused rollouts, stale observed generations, and conflicting
  field managers.
- Assign one owner per mutable field. When HPA or KEDA owns replicas, optimize its
  minimum, maximum, behavior, and targets rather than Deployment replicas. When
  VPA owns requests, optimize VPA policy rather than the pod template.
- Refuse changes when ownership is ambiguous, required metrics are stale, the
  source mapping is unresolved, or an earlier proposal has not converged.
- Add per-profile and global reconciliation concurrency, API rate limits,
  Prometheus query budgets, caching, and bounded cardinality.

### Phase 3: Forecasting and proactive scaling

- Add required SLI definitions for request rate, errors, latency, queue depth,
  saturation, and availability, expressed as validated PromQL with a target and
  missing-data policy.
- Support external leading indicators and planned events through additional
  PromQL signals and calendar entries.
- Train and backtest models only after minimum history and quality gates are met.
  Before then, publish observations but do not autonomously deliver changes.
- Compute the action horizon as Git delivery plus GitOps reconciliation, rollout,
  startup, and safety margin. Propose changes early enough to converge before
  predicted demand.
- Stage changes when necessary: pre-scale replicas first, wait for convergence,
  then increase per-pod requests if the resulting rollout remains schedulable.
- Scale down only after forecasts, SLOs, anomaly state, and cooldown all agree
  that the headroom is no longer needed.

### Phase 4: Controller-specific optimization

- Deployments: replicas, per-container requests, optional limit policy,
  HPA/KEDA parameters, rollout surge and unavailable settings, topology, and
  readiness-aware staging.
- StatefulSets: per-container requests by default. Replica changes require
  explicit opt-in, healthy PVCs, stable ordinals, topology feasibility, and a
  configured quorum floor.
- DaemonSets: per-container requests and rollout settings only; replica count is
  never managed.
- Jobs: per-container requests and bounded `parallelism`; never change
  `completions`, images, commands, or business behavior.
- CronJobs: job-template resources and bounded parallelism using schedule-aware
  forecasts; never rewrite the schedule or silently change concurrency semantics.
- Support every regular container independently while accounting for init and
  sidecar overhead. Preserve CPU limits by default. Memory limits may only
  increase or remain above configured p99 and OOM headroom.

### Phase 5: GitHub actuation and convergence

- Use a least-privilege GitHub App, short-lived installation tokens, shallow
  clones, base-SHA concurrency checks, retry and rate-limit handling, and secret
  redaction.
- Support three delivery policies:
  - `PullRequest`: always open or update a pull request.
  - `Direct`: commit to a configured branch after all validations.
  - `Policy`: open a pull request, auto-merge low-risk changes, and leave
    high-risk changes for approval.
- Scale-ups and bounded request increases can be eligible for automatic merge.
  Scale-downs, StatefulSet replica changes, large reductions, low-confidence
  forecasts, and changes during SLO burn always require approval.
- Render Raw YAML, Kustomize, or Helm before committing. Validate schemas, source
  ownership, diff scope, policy bounds, scheduler feasibility, and Kubernetes
  server-side dry-run where permissions permit.
- Make proposals idempotent using profile generation, source SHA, cluster
  snapshot hash, and recommendation hash. Allow only one active proposal per
  workload.
- Observe the Git commit, GitOps-controller synchronization, workload
  `observedGeneration`, rollout health, readiness, forecast error, and SLOs.
- Revert the exact optimizer commit when a delivered change causes readiness
  loss, unschedulability, increased OOMs, latency or error regression, or an
  error-budget breach. Never rewrite or reset branch history.
- Never combine a disruptive request change with a scale-down in one rollout.
  Dependent proposals must converge successfully between stages.

### Phase 6: Packaging and open-source readiness

- Provide Helm and Kustomize installations containing CRDs, HA controller and
  forecasting deployments, Services, probes, ServiceMonitors, NetworkPolicies,
  PodDisruptionBudgets, hardened security contexts, and sizing profiles.
- Publish multi-architecture images, SBOMs, signed provenance, vulnerability
  scans, reproducible builds, changelogs, upgrade notes, API compatibility
  policy, security policy, contribution guide, architecture decisions, and
  operational runbooks.
- Add generic examples for stateless services, stateful services, workers,
  DaemonSets, Jobs, and CronJobs. Shipyard-specific behavior must not exist in
  core control flow.

## Safety Rules

- A recommendation is never delivered if it cannot be placed on existing cluster
  capacity by the scheduler simulator.
- No scale-down is delivered during an active anomaly, SLO burn, incomplete
  rollout, degraded readiness, Git drift, or forecast uncertainty breach.
- PDBs are inputs but not sufficient rollout protection; the optimizer enforces
  its own availability and rollout budgets.
- Required topology, quorum, storage, priority, quota, and ownership constraints
  are hard constraints and cannot be traded for efficiency.
- Model-service failure cannot cause unsafe action. It activates the conservative
  deterministic fallback or holds the workload.
- Every decision is explainable through typed evidence, forecast version,
  feasibility results, source revision, cluster snapshot, and status conditions.
- Every Git change has a deterministic inverse and is monitored through a
  post-convergence safety window.

## Test Plan and Acceptance Criteria

### Correctness and integration

- Unit and property tests cover policy bounds, Kubernetes quantities,
  multi-container allocation, model responses, objective ordering, ownership,
  stability, and every controller-specific mutation.
- Fuzz tests cover CRD decoding and defaulting, Prometheus responses, YAML and
  Helm edits, Git diffs, and scheduler inputs.
- Golden tests verify comment-preserving Raw YAML edits, dedicated Kustomize
  patches, Helm values changes, and stable idempotent output.
- Scheduler differential tests compare simulated feasibility with kube-scheduler
  results across resource, affinity, taint, topology, PV, quota, init-container,
  and DaemonSet scenarios.
- Forecast tests use rolling-origin backtests, missing and stale data, regime
  changes, holidays, flash spikes, cold starts, and adversarial external signals.
- Integration tests use `envtest`, fake Prometheus and GitHub APIs, the real gRPC
  contract, and temporary Git repositories.
- End-to-end tests use kind for GitOps convergence and rollback, plus a KWOK-style
  environment for 1,000 nodes and 50,000 pods.
- Chaos tests terminate controller and model replicas, delay dependencies,
  corrupt individual model artifacts, exhaust API rate limits, and introduce
  stale caches.

### Quality and performance gates

- Critical optimizer, simulator, policy, and actuator packages maintain at least
  95 percent statement coverage; repository-wide coverage exceeds 85 percent.
- Cached single-workload reconciliation p95 is below two seconds.
- A full 1,000-node and 50,000-pod snapshot refresh completes within 60 seconds.
- Cached forecast inference p95 is below 200 milliseconds.
- Queues, goroutines, metric labels, training jobs, Prometheus queries, and stored
  history are strictly bounded.
- A model must achieve calibrated p90 interval coverage between 85 and 95 percent
  and improve seasonal-naive pinball loss by at least 15 percent before it can
  influence automatic Git delivery.
- No committed recommendation may be unschedulable according to the simulator.
- Automatic Git rollback must succeed for every induced readiness, scheduling,
  OOM, latency, error-rate, or error-budget regression scenario.

### Rollout

Release progressively through observe-only, pull-request-only, policy
auto-merge, and direct-commit stages. Promotion requires a burn-in period with
dashboards for recommendation accuracy, forecast calibration, convergence
latency, rollback rate, SLO impact, control-plane resource use, and estimated
waste reduction.
