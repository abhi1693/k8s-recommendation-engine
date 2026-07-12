# ADR 0001: Kubebuilder ApplicationProfile Controller

## Status

Accepted

## Context

The prototype starts one polling process per profile and loads each profile from a file. That duplicates Deployments, state volumes, Git clones, credentials, and process lifecycle configuration as profile count grows. A failure in one process is isolated, but Kubernetes has no API object that exposes profile validation, observed generation, reconciliation status, or the latest workload decisions.

The recommendation, state, Git proposal, and availability-recovery packages already form reusable domain services. The missing boundary is a Kubernetes-native lifecycle around those services.

## Decision

Use Kubebuilder v4 scaffolding with controller-runtime for a namespaced `ApplicationProfile` CRD in `k8s-recommendation-engine.io/v1alpha1`.

- One controller-runtime manager handles all profile objects in its configured control namespace.
- Each profile is reconciled independently and returns `RequeueAfter` for Prometheus polling.
- Generation-change predicates prevent status updates from creating reconcile loops.
- Errors return to controller-runtime for rate-limited retries; invalid specs wait for a new generation.
- The status subresource stores conditions and bounded workload/proposal summaries, not complete metric history.
- Persisted learning state defaults to a collision-safe `namespace/name` identity, with an explicit migration override for legacy profile keys.
- Leader election permits multiple manager replicas while keeping one active reconciler.
- Existing analysis, SQLite state, GitOps proposal, and failed-Pod recovery logic is invoked through a processor adapter.
- A shared Git worktree forces `max-concurrent-reconciles=1` until repository-keyed workspace and lock management is implemented.
- Pod deletion is not granted by the generated ClusterRole. Availability recovery continues to require an explicit RoleBinding in each target namespace.

Operator SDK was not selected because its Go projects use Kubebuilder/controller-runtime underneath. Its additional OLM, OperatorHub, and scorecard integration is not needed for the current GitOps deployment model.

## Consequences

- Adding a profile becomes creating one CR rather than deploying another controller.
- One malformed or unavailable profile no longer exits the manager or delays other profiles beyond configured queue concurrency.
- File-based `analyze`, `run`, and `backtest` commands remain available during migration.
- CR metric profiles move under `spec.metricProfiles`; the legacy file loader remains unchanged.
- The first controller release supports multiple profiles sharing one Prometheus endpoint, state database, and optionally one serialized Git worktree.
- A later change must add repository-keyed clone caches and locks before allowing concurrent Git reconciliation across repositories.
- Secondary Deployment and Pod watches can be added later for faster event-driven recovery; periodic reconciliation remains the correctness fallback.
