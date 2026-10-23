#!/usr/bin/env bash
set -euo pipefail

if [[ "${GITHUB_REF_TYPE:-}" != tag ]]; then
  echo 'Publish only from the immutable validated release tag.' >&2
  exit 1
fi
version=$(python3 -c 'import json; print(json.load(open("release.json"))["version"])')
[[ "${GITHUB_REF_NAME#v}" == "$version" ]]
[[ -z "${REQUESTED_VERSION:-}" || "${REQUESTED_VERSION#v}" == "$version" ]]
tag_commit=$(git rev-parse --verify "refs/tags/$GITHUB_REF_NAME^{commit}")
[[ "$tag_commit" == "$GITHUB_SHA" ]]
[[ "$(git rev-parse HEAD)" == "$GITHUB_SHA" ]]
