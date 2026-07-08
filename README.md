# K8s Recommendation Engine

Shipyard-first Kubernetes/K3s recommendation engine for predictive scaling through GitOps.

The current implementation is Phase 1 plus dry-run GitOps patch planning: it validates Shipyard workload ownership, autoscaler blockers, Prometheus metric availability, historical recommendation inputs, anomaly state, and the Fleet source fields that would be patched. It does not patch Kubernetes and does not write to Git.

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

## Run Continuously Without Git Changes

Use `run` for controller-like continuous reconciliation in dry-run mode. This reads Kubernetes and Prometheus, records learning state when `--state-db` is set, and prints recommendations every interval. It does not patch Kubernetes and does not write to Git.

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
In dry-run mode, `LAST OUTCOME` is usually `not_applied` because the controller intentionally did not write Git or patch Kubernetes.

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

Observation compares Git desired replicas/CPU/memory with the live Kubernetes Deployment spec and records convergence status in SQLite when `--state-db` is set.

Rollback the latest `k8s-recommendation-engine:` proposal commit on the configured default branch:

```bash
go run ./cmd/k8s-recommendation-engine proposal rollback \
  --git-worktree /path/to/home-lab \
  --branch master \
  --push \
  --allow-default-branch-push
```

Rollback uses `git revert --no-edit HEAD`; it does not reset history.

JSON output:

```bash
go run ./cmd/k8s-recommendation-engine analyze --output json
```

## Current Scope

- `shipyardhq/shipyardhq`
- `shipyardhq/shipyardhq-imgproxy`
- `shipyardhq/shipyardhq-worker`

The controller internals are generic: Shipyard is represented by configuration, not hardcoded control flow.
