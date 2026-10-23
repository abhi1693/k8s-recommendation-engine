#!/usr/bin/env bash
set -euo pipefail
image=${1:?image required}
platform=${2:?platform required}
version=${3:?version required}
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
case "$(uname -m)" in
  x86_64) native=linux/amd64 ;;
  aarch64|arm64) native=linux/arm64 ;;
  *) echo 'Unsupported native architecture' >&2; exit 1 ;;
esac
[[ "$platform" == "$native" ]] || { echo 'Smoke requires a native image' >&2; exit 1; }
[[ "$image" != -* ]]
python3 "$repo/scripts/ci/smoke-runtime.py" "$image" "$platform" "$version"
