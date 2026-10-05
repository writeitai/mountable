#!/bin/sh
# Build the four mountable binaries, SHA256SUMS, the npm package and the wheel
# for one version into dist/. Used by .github/workflows/release.yml and CI.
#   packaging/build.sh 1.2.3
set -eu
version=$1
root=$(cd "$(dirname "$0")/.." && pwd)
dist=$root/dist
rm -rf "$dist"
mkdir -p "$dist/bin"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/}
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-X main.version=$version" -o "$dist/bin/mountable-$os-$arch" ./cmd/mountable)
done
(cd "$dist/bin" && shasum -a 256 mountable-* > SHA256SUMS)

# npm: Node names the architectures x64 and arm64.
cp -R "$root/packaging/npm" "$dist/npm"
for os in linux darwin; do
  cp "$dist/bin/mountable-$os-amd64" "$dist/npm/bin/mountable-$os-x64"
  cp "$dist/bin/mountable-$os-arm64" "$dist/npm/bin/mountable-$os-arm64"
done
(cd "$dist/npm" && npm pkg set version="$version" && npm pack --pack-destination "$dist")

# PyPI: one py3-none-any wheel with the binaries under mountable/bin/.
cp -R "$root/packaging/pypi" "$dist/pypi"
mkdir -p "$dist/pypi/src/mountable/bin"
cp "$dist"/bin/mountable-*-* "$dist/pypi/src/mountable/bin/"
sed "s/^version = .*/version = \"$version\"/" "$root/packaging/pypi/pyproject.toml" > "$dist/pypi/pyproject.toml"
uv build --wheel --out-dir "$dist" "$dist/pypi"
