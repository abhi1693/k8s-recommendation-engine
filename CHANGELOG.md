# Changelog

## 0.3.6 (unreleased)

- Upgrade Kubernetes Go clients to 0.37.1, controller-runtime to 0.25.2, SQLite to 1.61.0, and all compatible transitive Go modules.
- Update the build toolchain and image to Go 1.27.3, controller-tools to 0.23.0, Kustomize to 5.8.3, and setup-envtest to 0.25.2.
- Retain kube-openapi at its last revision before the structured-merge-diff v7 migration (be32def86098, August 21). Kubernetes apimachinery 0.37.1 and controller-runtime 0.25.2 still require v6; current kube-openapi cannot compile with their public schema types. No module replacement or patched upstream code is used.
- Validate future dependency PRs with unit tests, static checks, code-generation freshness, and a disposable API-server integration test.
