#!/usr/bin/env bash
#
# Fail when a version's major does not match the module path in go.mod.
#
# Go's semantic import versioning: a module tagged v2+ must declare a path
# ending in /vN, and a v0/v1 module must not. A mismatch builds and tests fine
# inside this repo (the local source is used directly), but `go get` of the tag
# is rejected for every customer:
#
#   invalid version: module contains a go.mod file, so module path must match
#   major version ("github.com/Tencent-RTC/trtc-asr-sdk-go/v2")
#
# v2.0.0 shipped that way. This check runs from release.sh before tagging and
# from CI on every push.
#
# Usage:
#   scripts/check-module-path.sh            # checks SDKVersion in common/sdkinfo.go
#   scripts/check-module-path.sh 1.4.0      # or v1.4.0

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
  VERSION="$(sed -n 's/^[[:space:]]*SDKVersion = "\(.*\)"$/\1/p' common/sdkinfo.go)"
fi
VERSION="${VERSION#v}"

if [[ ! "$VERSION" =~ ^([0-9]+)\.[0-9]+\.[0-9]+ ]]; then
  echo "check-module-path: cannot parse version '$VERSION'" >&2
  exit 2
fi
MAJOR="${BASH_REMATCH[1]}"

MODULE="$(sed -n 's/^module[[:space:]]\{1,\}\([^[:space:]]*\).*/\1/p' go.mod | head -n1)"
if [[ -z "$MODULE" ]]; then
  echo "check-module-path: no module line in go.mod" >&2
  exit 2
fi

SUFFIX=""
if [[ "$MODULE" =~ /v([0-9]+)$ ]]; then
  SUFFIX="${BASH_REMATCH[1]}"
fi

if (( MAJOR >= 2 )); then
  if [[ "$SUFFIX" != "$MAJOR" ]]; then
    echo "check-module-path: version $VERSION needs module path ending in /v$MAJOR," >&2
    echo "check-module-path: go.mod declares '$MODULE'." >&2
    echo "check-module-path: either keep the release on v1.x, or move the module to" >&2
    echo "check-module-path: '${MODULE%/v*}/v$MAJOR' and update every import." >&2
    exit 1
  fi
elif [[ -n "$SUFFIX" ]]; then
  echo "check-module-path: version $VERSION must not use a /v$SUFFIX module path ('$MODULE')." >&2
  exit 1
fi

echo "check-module-path: ok ($MODULE @ v$VERSION)"
