#!/bin/sh
# Build the mountable binaries, the GitHub release archives with SHA256SUMS,
# the npm package and the wheel for one version into dist/. Used by
# .github/workflows/release.yml and CI.
#   packaging/build.sh 1.2.3
# The Windows binaries are always built into dist/bin/, but go into the
# release archives, the npm package and the wheel only with
# MOUNTABLE_WINDOWS_PACKAGES=1: Windows ships once its qualification passes
# (fsit design/designs/windows-mount.md §7).
set -eu
version=$1
root=$(cd "$(dirname "$0")/.." && pwd)
dist=$root/dist
rm -rf "$dist"
mkdir -p "$dist/bin" "$dist/release"

# Every binary and package carries LICENSE and THIRD_PARTY_NOTICES.
windows_packages=${MOUNTABLE_WINDOWS_PACKAGES:-0}
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*} arch=${target#*/}
  exe=""
  [ "$os" = windows ] && exe=.exe
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$dist/bin/mountable-$os-$arch$exe" ./cmd/mountable)
  [ "$os" = windows ] && [ "$windows_packages" != 1 ] && continue
  # GitHub release: mountable-<os>-<arch>.tar.gz holding the executable,
  # LICENSE and THIRD_PARTY_NOTICES.
  stage=$dist/stage/$os-$arch
  mkdir -p "$stage"
  cp "$dist/bin/mountable-$os-$arch$exe" "$stage/mountable$exe"
  cp "$root/LICENSE" "$root/THIRD_PARTY_NOTICES" "$stage/"
  tar -C "$stage" -czf "$dist/release/mountable-$os-$arch.tar.gz" "mountable$exe" LICENSE THIRD_PARTY_NOTICES
done
rm -rf "$dist/stage"
(cd "$dist/release" && shasum -a 256 mountable-*.tar.gz > SHA256SUMS)

# npm: Node names the architectures x64 and arm64.
cp -R "$root/packaging/npm" "$dist/npm"
cp "$root/THIRD_PARTY_NOTICES" "$dist/npm/"
for os in linux darwin; do
  cp "$dist/bin/mountable-$os-amd64" "$dist/npm/bin/mountable-$os-x64"
  cp "$dist/bin/mountable-$os-arm64" "$dist/npm/bin/mountable-$os-arm64"
done
if [ "$windows_packages" = 1 ]; then
  cp "$dist/bin/mountable-windows-amd64.exe" "$dist/npm/bin/mountable-win32-x64.exe"
  cp "$dist/bin/mountable-windows-arm64.exe" "$dist/npm/bin/mountable-win32-arm64.exe"
fi
(cd "$dist/npm" && npm pkg set version="$version" && npm pack --pack-destination "$dist")

# PyPI: one py3-none-any wheel with the binaries under mountable/bin/.
cp -R "$root/packaging/pypi" "$dist/pypi"
cp "$root/THIRD_PARTY_NOTICES" "$dist/pypi/"
mkdir -p "$dist/pypi/src/mountable/bin"
for binary in "$dist"/bin/mountable-*-*; do
  case $binary in
  *.exe) [ "$windows_packages" = 1 ] || continue ;;
  esac
  cp "$binary" "$dist/pypi/src/mountable/bin/"
done
sed "s/^version = .*/version = \"$version\"/" "$root/packaging/pypi/pyproject.toml" > "$dist/pypi/pyproject.toml"
uv build --wheel --out-dir "$dist" "$dist/pypi"
