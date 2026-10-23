# Repository automation

Common CI and image mechanics are maintained in [abhi1693/actions](https://github.com/abhi1693/actions).
Workflows use the released `@v1` reference for centrally maintained compatible updates.
See the [shared release history](https://github.com/abhi1693/actions/releases) for changes.
Repository-owned scripts, triggers, scanner exceptions and application smoke checks
remain alongside the application. Image manifests declare components rather than
copying workflow steps.

The manifest builds both native Linux/AMD64 and Linux/ARM64 images. PRs never
publish. Releases verify tag/source/version identity before the shared strict
SBOM, vulnerability/secret scan and runtime checks, with signed GitHub attestations
enabled. The repository-owned smoke uses only disposable no-egress HTTPS/Git/API
fixtures; see the root README for the local command and publication boundary.
