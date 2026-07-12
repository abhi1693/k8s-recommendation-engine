# ADR 0002: Explicit Helm Values Source Mappings

## Status

Accepted

## Context

The recommendation engine can observe a live Deployment and update a literal Deployment manifest in Git. Fleet-managed applications frequently render that Deployment from an external Helm chart, so the writable source is a chart-specific `values.yaml` key such as `replicaCount`, `resources.requests.cpu`, or `login.resources.requests.memory`. The values document has no Kubernetes resource identity, and different chart components can share one file.

## Decision

Keep `workload.sourceFile` as the Git file selector and add optional `workload.helmValues.paths` mappings for replicas, CPU request, and memory request. Each mapping is an array of exact YAML mapping-key segments.

- The presence of `helmValues` selects Helm-values mode; its absence preserves the existing Kubernetes-manifest behavior.
- `targetRef` continues to identify the live Deployment used for observation and recommendations.
- Patch changes retain their semantic Kubernetes field and separately record their Helm source path.
- Mapped leaves must already exist as scalars in one mapping-only YAML document.
- Git values must semantically match the live Deployment before planning; replicas use integer comparison and resources use Kubernetes quantity comparison.
- Proposal replay uses compare-and-set against the planned current value.
- Duplicate, prefix-overlapping, mixed-format, aliased, merged, stale, or worktree-escaping sources are rejected.
- Multiple workloads may share one values file only with disjoint paths; their changes are merged into one deterministic file proposal.

## Alternatives Considered

- Dotted strings are shorter but ambiguous for literal keys containing dots and require a custom escaping language.
- JSONPath or JSON Pointer is more expressive than the initial need and would allow sequence and dynamic selection semantics that are harder to validate safely.
- Adding missing values keys is convenient but makes a misspelling look successful even when Helm ignores it.
- Rendering and editing chart output would fight Fleet because the rendered Deployment is not the Git source of truth.

## Consequences

- Existing profiles and raw manifest proposals remain compatible.
- Charts with different values layouts can opt in without engine-specific templates.
- Shared values files such as a server plus worker or login component are supported safely.
- Structured YAML encoding preserves ordering and comments but may normalize scalar formatting or indentation in a proposed file.
- The configured file must be the effective highest-precedence values input.
- Arrays, embedded values block scalars, transforms, multi-container selection, and non-Deployment targets require later explicit designs.
- Ownership is validated within one profile, but operators must avoid assigning the same Git scalar to separate profiles until repository-wide ownership coordination exists.
