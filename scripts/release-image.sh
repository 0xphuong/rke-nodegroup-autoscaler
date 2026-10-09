#!/usr/bin/env bash
# release-image.sh — build the provider image locally (multi-arch, docker buildx) and push it to Docker Hub.
#
# The same thing CI does on a vX.Y.Z tag, for when CI is not an option or a build is needed before tagging.
#
# Usage:
#   scripts/release-image.sh [--version X.Y.Z] [--platforms linux/amd64,linux/arm64] [--latest] [--chart]
#                            [--builder NAME] [--skip-tests] [--allow-dirty] [--force] [--dry-run] [--yes]
#
#   --version      image tag (default: appVersion of the chart, which must equal the chart version)
#   --platforms    default linux/amd64,linux/arm64
#   --latest       also tag :latest
#   --chart        also package the chart and push it to oci://registry-1.docker.io/<user> (needs
#                  `helm registry login registry-1.docker.io` first)
#   --builder      buildx builder to use (must support multi-platform, e.g. the docker-container driver)
#   --skip-tests   do not run `make test` first
#   --allow-dirty  build from uncommitted changes (asked for interactively otherwise)
#   --force        overwrite a version that already exists on Docker Hub (refused by default)
#   --dry-run      print what would be done, build and push nothing
#   --yes          do not ask for confirmation
#
# Needs `docker login` (Docker Hub user with write access to the repository).
# Env: IMAGE (default docker.io/binhphuong/rke-nodegroup-autoscaler), CHART_REPO (default oci://registry-1.docker.io/binhphuong)
set -euo pipefail

IMAGE="${IMAGE:-docker.io/binhphuong/rke-nodegroup-autoscaler}"
CHART_REPO="${CHART_REPO:-oci://registry-1.docker.io/binhphuong}"
CHART_DIR="charts/rke-nodegroup-autoscaler"
VERSION="" PLATFORMS="linux/amd64,linux/arm64" LATEST=0 CHART=0 BUILDER="" TESTS=1 DIRTY_OK=0 FORCE=0 DRY=0 YES=0

die() { echo "ERROR: $*" >&2; exit 2; }
usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit "${1:-0}"; }
run() { echo "+ $*"; [ "$DRY" = 1 ] || "$@"; }
confirm() {
  [ "$YES" = 1 ] && return 0
  local a; read -r -p "$1 [y/N] " a; [ "$a" = y ] || [ "$a" = Y ]
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --platforms) PLATFORMS="${2:?}"; shift 2 ;;
    --latest) LATEST=1; shift ;;
    --chart) CHART=1; shift ;;
    --builder) BUILDER="${2:?}"; shift 2 ;;
    --skip-tests) TESTS=0; shift ;;
    --allow-dirty) DIRTY_OK=1; shift ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY=1; shift ;;
    --yes|-y) YES=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 2 ;;
  esac
done

cd "$(git rev-parse --show-toplevel)"
command -v docker >/dev/null || die "docker not found"
docker buildx version >/dev/null 2>&1 || die "docker buildx not available"

chart_version=$(sed -n 's/^version: *//p' "$CHART_DIR/Chart.yaml")
app_version=$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/p' "$CHART_DIR/Chart.yaml")
[ "$chart_version" = "$app_version" ] || die "Chart.yaml version ($chart_version) != appVersion ($app_version)"
VERSION="${VERSION:-$app_version}"
VERSION="${VERSION#v}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version '$VERSION' is not X.Y.Z"
[ "$VERSION" = "$app_version" ] || echo "WARNING: image $VERSION differs from the chart appVersion $app_version" >&2

# what goes into the binary's --version: X.Y.Z only for a clean tree at the tag vX.Y.Z
sha=$(git rev-parse --short HEAD)
dirty=""
[ -z "$(git status --porcelain --untracked-files=normal)" ] || dirty="-dirty"
build_version="$VERSION"
if [ -n "$dirty" ] || [ "$(git tag --points-at HEAD | grep -cx "v$VERSION")" = 0 ]; then
  build_version="$VERSION+g$sha$dirty"
fi

if [ -n "$dirty" ] && [ "$DIRTY_OK" = 0 ]; then
  git status --short
  confirm "Working tree has uncommitted changes; build $IMAGE:$VERSION from them?" || die "aborted"
fi

if docker buildx imagetools inspect "$IMAGE:$VERSION" >/dev/null 2>&1; then
  [ "$FORCE" = 1 ] || die "$IMAGE:$VERSION already exists on the registry; pick a new version or pass --force"
  echo "WARNING: overwriting the existing $IMAGE:$VERSION" >&2
fi

tags=(-t "$IMAGE:$VERSION")
[ "$LATEST" = 1 ] && tags+=(-t "$IMAGE:latest")
builder=()
[ -n "$BUILDER" ] && builder=(--builder "$BUILDER")

echo "image:      $IMAGE:$VERSION$([ "$LATEST" = 1 ] && echo " (+ latest)")"
echo "platforms:  $PLATFORMS"
echo "binary:     version $build_version"
echo "chart:      $([ "$CHART" = 1 ] && echo "push $chart_version to $CHART_REPO" || echo "not pushed")"
confirm "Build and push?" || die "aborted"

if [ "$TESTS" = 1 ]; then
  run make test
fi

run docker buildx build ${builder[@]+"${builder[@]}"} --platform "$PLATFORMS" --build-arg "VERSION=$build_version" \
  "${tags[@]}" --push .

if [ "$CHART" = 1 ]; then
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  run helm package "$CHART_DIR" -d "$tmp"
  run helm push "$tmp/rke-nodegroup-autoscaler-$chart_version.tgz" "$CHART_REPO"
fi

if [ "$DRY" = 0 ]; then
  echo
  docker buildx imagetools inspect "$IMAGE:$VERSION" | sed -n '1,/^$/p'
fi
