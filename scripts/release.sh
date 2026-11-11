#!/usr/bin/env bash
#
# Cut a release: bump SDKVersion, stamp CHANGELOG, commit, and tag.
#
# Usage:
#   scripts/release.sh 1.1.0
#   scripts/release.sh 1.1.0 --dry-run
#
# Does not push. After review:
#   git push origin HEAD && git push origin v1.1.0

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-}"
DRY_RUN=0
shift || true
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: scripts/release.sh <major.minor.patch> [--dry-run]" >&2
  exit 2
fi

DATE="$(date +%F)"
TAG="v${VERSION}"

if git rev-parse "$TAG" >/dev/null 2>&1; then
  echo "tag $TAG already exists" >&2
  exit 1
fi

CURRENT="$(sed -n 's/^[[:space:]]*SDKVersion = "\(.*\)"$/\1/p' common/sdkinfo.go)"
if [[ -z "$CURRENT" ]]; then
  echo "cannot read SDKVersion from common/sdkinfo.go" >&2
  exit 1
fi

stamp_changelog() {
  python3 - "$VERSION" "$DATE" <<'PY'
import sys
from pathlib import Path
version, date = sys.argv[1], sys.argv[2]
path = Path("CHANGELOG.md")
text = path.read_text(encoding="utf-8")
heading = f"## [{version}] - {date}"
if heading in text:
    sys.exit(0)
old = "## [未发布]"
if old not in text:
    raise SystemExit("CHANGELOG.md has no '## [未发布]' section to stamp")
# Keep an empty Unreleased section so the next cycle has a place to write.
replacement = f"## [未发布]\n\n## [{version}] - {date}"
path.write_text(text.replace(old, replacement, 1), encoding="utf-8")
PY
}

echo "==> $CURRENT -> $VERSION"

# A tag whose major does not match go.mod's module path cannot be fetched by
# `go get` at all (v2.0.0 shipped that way); refuse before anything is written.
"$ROOT/scripts/check-module-path.sh" "$VERSION"

if [[ $DRY_RUN -eq 1 ]]; then
  echo "would update common/sdkinfo.go SDKVersion"
  echo "would stamp CHANGELOG.md as ## [$VERSION] - $DATE"
  echo "would commit and tag $TAG"
  exit 0
fi

perl -i -pe "s/SDKVersion = \"$CURRENT\"/SDKVersion = \"$VERSION\"/" common/sdkinfo.go
stamp_changelog

git add common/sdkinfo.go CHANGELOG.md
git commit -m "chore: release $VERSION"
git tag -a "$TAG" -m "Release $VERSION"

echo "tagged $TAG. push with:"
echo "  git push origin HEAD && git push origin $TAG"
