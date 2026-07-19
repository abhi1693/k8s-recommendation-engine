# K8s Recommendation Engine

Shipyard-first Kubernetes/K3s recommendation engine for predictive scaling through GitOps.

The default analysis path is read-only. Proposal mode can write gated changes through GitOps, and the separately gated availability-recovery mode can recreate a failed Pod directly; it never patches live workload resources.

## Kubernetes Controller

The production controller uses [Kubebuilder](https://book.kubebuilder.io/) project scaffolding and [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime). One manager watches any number of namespaced `ApplicationProfile` custom resources and gives each profile an independent work-queue item, retry history, reconcile interval, timeout, status, and failure boundary.

Generate the API code and manifests:

```bash
make manifests generate
```

Install the CRD and run the controller against the current kubeconfig:

```bash
make install
go run ./cmd/k8s-recommendation-engine controller \
  --watch-namespace k8s-recommendation-engine-system \
  --prometheus-url http://127.0.0.1:9090 \
  --state-db .state/controller.db \
  --state-retention 336h \
  --reconcile-timeout 5m
```

Create a profile and inspect its bounded status summary:

```bash
kubectl apply -f config/samples/k8s-recommendation-engine_v1alpha1_applicationprofile.yaml
kubectl get applicationprofiles -A
kubectl get applicationprofile -n k8s-recommendation-engine-system example -o yaml
```

CR profiles place `metricProfiles` under `spec`, unlike the legacy file format where it is a top-level field. `spec.suspend: true` stops one profile without stopping the manager. `spec.reconcileInterval` overrides the controller default for that profile. Persisted state is keyed by `namespace/name`; `spec.stateKey` exists only to preserve a legacy profile's prior SQLite history during migration.

`Ready` reports whether the latest reconciliation completed, while `ProposalReady` independently reports whether the Git proposal path is ready, waiting, or blocked. This keeps an intentional safety or batch hold from making a healthy workload look degraded. `kubectl get applicationprofiles` displays both conditions.

Controller mode can run the existing GitOps proposal pipeline with `--git-worktree`, `--mode propose`, and the normal proposal flags. Reconciles are intentionally limited to one at a time while profiles share a worktree. This prevents concurrent fetch/commit/push races; keyed per-repository workspaces are the next scaling boundary. Direct Pod recovery still requires both `--availability-recovery`, workload policy opt-in, persisted state, and a namespace-local Pod-delete Role.

Leader election is disabled by default and is unnecessary for the supplied single-replica `Recreate` Deployment. Multi-replica installations can enable `--leader-elect`; the default lease timings are a 30-second lease, 20-second renewal deadline, and 5-second retry period, and each value has a corresponding `--leader-election-*` flag.

## Git Source Mappings

`sourceFile` normally points to a Kubernetes manifest containing the target `Deployment` or `StatefulSet`. For a Helm-managed workload, point it at the effective values file and add `helmValues.paths`. Each path is an ordered list of YAML mapping keys, so chart-specific names and keys containing dots or hyphens remain unambiguous.

Two Deployments can safely share one values file when they own disjoint paths:

```yaml
spec:
  namespace: zitadel
  git:
    provider: github
    mode: fleet
    repository: https://github.com/example/home-lab.git
    branch: master
    basePath: kubernetes/apps/zitadel
  workloads:
    - name: server
      targetRef:
        apiVersion: apps/v1
        kind: Deployment
        name: zitadel
      sourceFile: values.yaml
      helmValues:
        paths:
          replicas: [replicaCount]
          cpuRequest: [resources, requests, cpu]
          memoryRequest: [resources, requests, memory]
      metricProfileRef: resource-only
      scaling:
        replicas: true
        cpu: true
        memory: true

    - name: login
      targetRef:
        apiVersion: apps/v1
        kind: Deployment
        name: zitadel-login
      sourceFile: values.yaml
      helmValues:
        paths:
          replicas: [login, replicaCount]
          cpuRequest: [login, resources, requests, cpu]
          memoryRequest: [login, resources, requests, memory]
      metricProfileRef: resource-only
      scaling:
        replicas: true
        cpu: true
        memory: true
```

The semantic fields remain workload replicas and container requests; the paths only describe where those fields live in Git. Patch planning requires every mapped leaf to already exist as a scalar and to match the live workload baseline. It compares Kubernetes quantities semantically, so values such as `0.5` and `500m` are equivalent. Missing keys, YAML aliases/merges, source drift, overlapping ownership, mixed manifest/Helm mappings, and concurrent edits block the proposal instead of creating or overwriting a value.

The controller exposes the bounded result under `status.workloads[*].patch`. Mapping and source errors mark that workload blocked and set the profile's `Degraded` condition while leaving the full diff out of the CR status.

The values file must be the effective highest-precedence Helm input. A later `valuesFrom`, `--set`, or inline override can prevent Git and the live workload from converging. Keep one `ApplicationProfile` owner per Git scalar; ownership cannot currently be coordinated across separate profile resources.

Initial Helm support intentionally covers one mapping-only YAML document with existing scalar leaves. Structured encoding preserves keys and comments but can normalize blank lines or indentation on the first proposal; the displayed plan diff uses the raw source so that formatting churn is visible before a commit. Sequence traversal, embedded `HelmChart.spec.valuesContent`, chart-specific transforms, and multi-container resource selection remain out of scope.

Run the controller lifecycle test against an envtest API server:

```bash
make test-integration
```

## Analyze Shipyard

Port-forward Prometheus:

```bash
kubectl -n cattle-monitoring-system port-forward svc/rancher-monitoring-prometheus 9090:9090
```

Run the analyzer:

```bash
go run ./cmd/k8s-recommendation-engine analyze \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090
```

Pretty output:

```bash
go run ./cmd/k8s-recommendation-engine analyze \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --output pretty
```

Persistent learning:

```bash
go run ./cmd/k8s-recommendation-engine analyze \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --history-window 6h \
  --history-step 5m \
  --state-db .state/k8s-recommendation-engine.db \
  --output pretty
```

The first run creates the SQLite state database and records the current learned envelopes. Later runs show prior persisted recommendation and signal counts in each workload's learning section.
When `--state-db` is enabled, later runs also evaluate the latest prior recommendation and show `LAST OUTCOME` in summary output.

Persisted learning and operational rows are retained for 14 days by default and pruned at most once per hour. Override this with `--state-retention`; size the PVC for the workload count, reconcile cadence, and selected retention. Pruning makes SQLite reuse freed pages but does not shrink an existing file. To reclaim disk from a database that grew before retention was enabled, stop every writer, back up the file, run an offline `VACUUM`, and then restart the controller.

## Forecast Horizons

For forecastable signals, the analyzer computes explicit horizons from the Prometheus range samples:

- `next_15m`
- `next_30m`
- `next_1h`
- `same_time_yesterday` baseline
- `same_weekday` baseline
- rolling trend slope per hour

The `next_30m` request-rate upper band is used as a proactive traffic input for replica planning. The engine still requires the normal anomaly, pressure, stability, rollout, and proposal gates before a Git proposal is allowed.

Pretty reports render this as:

```text
forecast: next_30m request_rate=4.2 p95_band=3.1-5.0 confidence=0.82
```

## Seasonality Learning

When `--state-db` is enabled, each reconcile stores workload signal observations in SQLite hourly buckets:

- traffic p50/p95/max by hour
- CPU p50/p95/max by hour
- memory p50/p95/max by hour
- latency p50/p95/max by traffic band
- same day-of-week and weekday/weekend buckets

Seasonality is used conservatively. It can hold replica, CPU, or memory reductions when the current hour's historical p95 is materially higher than the current learned envelope. This prevents scale-down decisions from being based only on a quiet moment right before a recurring busy period.

## Multi-Signal Replica Decisions

Replica count is decided by a combined score rather than a single metric. The scorer considers:

- traffic forecast
- latency pressure
- error rate pressure
- concurrent request pressure
- CPU pressure
- memory pressure
- rollout, PDB, configured minimum, and availability floors
- prior success or failure of replica recommendations recorded in SQLite

The report exposes the score, basis, floor, and contributing components. Example:

```text
replica decision: score=0.60 basis=traffic_forecast+latency, availability_floor floor=2 components=traffic_forecast=0.25/pressure,latency=0.35/pressure,availability_floor=0.00/floor
```

## Backtest Mode

Use `backtest` to replay Prometheus history and measure whether the predictive policy would have beaten a reactive scaler for the same workload profile.

```bash
go run ./cmd/k8s-recommendation-engine backtest \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --window 7d \
  --step 5m
```

Backtest answers:

- whether the engine would have scaled before detected spikes
- how much compute it would save versus observed capacity, reported as replica-hours
- how often it would over-provision or under-provision compared with a reactive current-signal baseline
- how many Git proposal commits it would create after stability gating

The report separates `proactiveScaleBeforeSpikes` from `coveredByExistingCapacity`. A spike is only counted as proactive when the predictive replay scaled up before the spike; holding already-existing capacity is counted separately.

JSON output is available for automation:

```bash
go run ./cmd/k8s-recommendation-engine backtest \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --window 7d \
  --output json
```

## Workload Guardrails

Each workload can set per-resource change bounds in its profile. `minChangePercent` suppresses CPU or memory request recommendations whose absolute change is smaller than the configured percentage of the current request, which prevents noisy proposal commits such as `76Mi -> 77Mi`.

```yaml
bounds:
  cpu:
    minChangePercent: 10
  memory:
    minChangePercent: 5
```

`minChangePercent` is a proposal gate, not a step size. The engine still computes the learned target from observed history and prior accuracy; it only suppresses writing a Git proposal when the difference from the current request is too small to be worth rolling out.

Proposal frequency can also be limited per workload. Unset or `0` means unlimited for that limit.

```yaml
policy:
  maxProposalsPerHour: 1
  maxProposalsPerDay: 4
  availabilityRecovery:
    enabled: true
    failureGracePeriod: 2m
    cooldown: 5m
    maxAttemptsPerHour: 6
  safety:
    allowAutoCommit: [low_risk, medium_risk]
    maxDecreaseRisk: medium_risk
    urgentBypassAllowed: true
  confidence:
    minAutoCommit: 0.75
```

Proposal commits are grouped by default. When `--mode propose --proposal-kind commit` is used, stable applyable patch plans are stored in the SQLite batch queue and only become commit-eligible after the proposal batch window elapses. The current recommendation run is recorded after proposal delivery is known, so a batch-, safety-, or Git-blocked recommendation is never mistaken for a pushed change that should already be live. The default window is `15m`, which reduces one-commit-per-reconcile noise and lets multiple workload changes land in one reviewable Git commit.

```bash
go run ./cmd/k8s-recommendation-engine run \
  --mode propose \
  --proposal-kind commit \
  --proposal-batch-window 15m \
  --state-db .state/k8s-recommendation-engine.db
```

Set `--proposal-batch-window 0` to restore immediate commit behavior. A non-zero batch window requires `--state-db` because the pending batch must survive reconcile loops and process restarts.

Inspect pending grouped proposal items without touching Kubernetes or Git:

```bash
go run ./cmd/k8s-recommendation-engine proposal batch status \
  --state-db .state/k8s-recommendation-engine.db \
  --proposal-batch-window 15m
```

The status output shows each pending workload, first seen time, ready time, remaining wait, change count, and why the item is still waiting. Temporary safety, confidence, rollout, and outcome gates preserve the existing batch timer; a materially changed recommendation resets it. Use `--output json` for automation.

The batch window is bypassed for urgent surge protection. If a workload or shared traffic signal has an active request-rate, latency, error-rate, or concurrency anomaly and the proposal increases replicas, CPU, or memory, the commit can be created immediately. Decreases never bypass the batch window. Set `policy.safety.urgentBypassAllowed: false` for a workload when even urgent increases must wait for the batch window.

Before writing a proposal, the engine also checks the live workload rollout state. A proposal is blocked while the workload generation is still pending, updated/ready/available replicas have not caught up, unavailable replicas exist, or selected Pods are terminating, pending, unready, or have incomplete init containers. This prevents the controller from stacking new recommendations on top of an app that Fleet or Kubernetes has not finished applying yet.

Every recommendation also gets a safety classification before any Git proposal is written:

- `low_risk`
- `medium_risk`
- `high_risk`

Safety considers resource decrease size, prior forecast accuracy, workload health, rollout history, memory headroom, and traffic anomaly state. By default, `low_risk` and `medium_risk` recommendations can be proposed automatically after the normal stability, rollout, budget, and Git gates pass. `policy.safety.allowAutoCommit` overrides that allow-list per workload, and `policy.safety.maxDecreaseRisk` blocks auto-commit when the resource decrease factor is riskier than the configured maximum.

Confidence also decays automatically when Prometheus data quality weakens. Sparse range history, stale samples, noisy history, missing current samples, and query errors reduce the final confidence score. A recommendation with an actual resource or replica change is blocked when the adjusted confidence is below `policy.confidence.minAutoCommit`; the default is `0.75`.

## Availability Recovery

When a Deployment has fewer ready replicas than desired and one of its selected Pods has a non-zero terminated container or a current `CrashLoopBackOff`, the engine marks an availability emergency. It can use retained pre-crash memory history, recommend a bounded memory-request increase, and raise the replica floor by one up to the configured maximum. Emergency Git proposals bypass confidence, safety, rollout, stability, budget, and batch delays only when the workload opts in and every proposed change is an increase.

Direct Pod recreation requires both `policy.availabilityRecovery.enabled: true` and the `--availability-recovery` command flag. It also requires `--state-db` and namespace-scoped `delete` permission on Pods.

```bash
go run ./cmd/k8s-recommendation-engine run \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --state-db .state/k8s-recommendation-engine.db \
  --availability-recovery
```

The controller rechecks the Pod immediately before acting, waits for `failureGracePeriod`, and deletes at most one failed Pod per workload per reconcile with UID and resource-version preconditions. `cooldown` and `maxAttemptsPerHour` are persisted in SQLite. Kubernetes recreates the deleted Pod through its owning controller; CPU, memory, and replica changes still flow through GitOps.

Reports also convert recommendations into waste/savings units. CPU is shown as core-hours, memory as GiB-hours, and replicas as replica-hours. Hourly reduction is projected over `730h` for the monthly estimate. Positive values mean reduced requested capacity; negative values mean the recommendation intentionally adds capacity.

## Run Continuously Without Git Changes

Use `run` for controller-like continuous reconciliation in dry-run mode. This reads Kubernetes and Prometheus, records learning state when `--state-db` is set, and prints recommendations every interval. Without `--availability-recovery`, it does not patch Kubernetes or write to Git.

```bash
go run ./cmd/k8s-recommendation-engine run \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --history-window 6h \
  --history-step 5m \
  --state-db .state/k8s-recommendation-engine.db \
  --interval 5m \
  --output summary
```

Use `--output pretty` when you want the full detailed report for a reconcile cycle.
In dry-run mode, `LAST OUTCOME` is usually `dry_run_not_applied` because the controller intentionally did not write Git or patch Kubernetes. In propose mode, `not_applied` is reserved for a recommendation that was included in a successfully pushed proposal commit.

Do not pass `--git-worktree` if you want no Fleet source inspection at all. If `--git-worktree` is set, the app still only builds a dry-run patch plan from the local checkout; it does not commit.

Include Fleet source patch planning with a local checkout:

```bash
go run ./cmd/k8s-recommendation-engine analyze \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --history-window 6h \
  --history-step 5m \
  --git-worktree /tmp/home-lab-shipyard
```

Create and push a gated proposal commit directly to the configured default branch:

```bash
go run ./cmd/k8s-recommendation-engine analyze \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --history-window 6h \
  --history-step 5m \
  --state-db .state/k8s-recommendation-engine.db \
  --git-worktree /path/to/home-lab \
  --mode propose \
  --proposal-kind commit \
  --proposal-branch master \
  --proposal-push \
  --allow-default-branch-push \
  --output actions
```

`--proposal-push` publishes the local proposal commit. `--allow-default-branch-push` is required when the target is the configured default branch.

Observe whether Fleet has applied the pushed Git state to the cluster:

```bash
go run ./cmd/k8s-recommendation-engine proposal observe \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --history-window 6h \
  --history-step 5m \
  --state-db .state/k8s-recommendation-engine.db \
  --git-worktree /path/to/home-lab \
  --output text
```

Observation compares Git desired replicas/CPU/memory with the live Kubernetes workload spec and records convergence status in SQLite when `--state-db` is set.

Rollback the latest `k8s-recommendation-engine:` proposal commit on the configured default branch:

```bash
go run ./cmd/k8s-recommendation-engine proposal rollback \
  --git-worktree /path/to/home-lab \
  --branch master \
  --push \
  --allow-default-branch-push
```

Rollback uses `git revert --no-edit HEAD`; it does not reset history.

Automatically roll back the latest proposal only after Fleet has applied it and the applied workload is now regressed or unsafe:

```bash
go run ./cmd/k8s-recommendation-engine proposal auto-rollback \
  --config configs/shipyard-profile.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --history-window 6h \
  --history-step 5m \
  --state-db .state/k8s-recommendation-engine.db \
  --git-worktree /path/to/home-lab \
  --branch master \
  --push \
  --allow-default-branch-push
```

Auto rollback first runs live analysis and convergence observation. It only creates a rollback commit when Git desired state matches the live workload and the observed outcome is `regressed` or `unsafe`. Pending Fleet convergence, drift, dirty Git worktrees, and missing proposal commits block automatic rollback.

JSON output:

```bash
go run ./cmd/k8s-recommendation-engine analyze --output json
```

## Current Scope

- `shipyardhq/shipyardhq`
- `shipyardhq/shipyardhq-imgproxy`
- `shipyardhq/shipyardhq-worker`

The controller internals are generic: Shipyard is represented by configuration, not hardcoded control flow.
