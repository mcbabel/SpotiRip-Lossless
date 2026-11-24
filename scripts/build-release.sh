#!/usr/bin/env bash
set -euo pipefail

project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$project_dir"

target_os=$(go env GOOS)
target_arch=$(go env GOARCH)
if [[ $target_os != linux && $target_os != windows ]] || [[ $target_arch != amd64 ]]; then
    printf 'The release build requires Linux or Windows on x86-64.\n' >&2
    exit 1
fi
if [[ $(go env CGO_ENABLED) != 1 ]]; then
    printf 'The release build requires CGo.\n' >&2
    exit 1
fi
if [[ $target_os == $(go env GOHOSTOS) && $target_arch == $(go env GOHOSTARCH) ]]; then
    go test ./cmd/... ./internal/...
else
    printf 'Cross-building %s-%s; target tests run separately.\n' "$target_os" "$target_arch"
fi

name="spotirip-lossless-$target_os-$target_arch"
stage="dist/$name"
archive="$stage.tar.gz"
rm -rf -- "$stage" "$archive" "$archive.sha256"
mkdir -p "$stage"

executable=spotirip-lossless
ldflags='-s -w -linkmode=external'
if [[ $target_os == windows ]]; then
    executable+=.exe
else
    ldflags+=' -extld=gcc'
fi
go build -tags=nodsp -trimpath -ldflags="$ldflags" -o "$stage/$executable" ./cmd/spotirip-lossless
if [[ $target_os == linux ]]; then
    python3 - "$stage/$executable" <<'PY'
import subprocess
import sys

linked = subprocess.check_output(["ldd", sys.argv[1]], text=True)
forbidden = [name for name in ["libFLAC", "libmpg123", "libasound", "libvorbis", "libogg", "libstdc++", "libgcc_s"] if name in linked]
if forbidden:
    raise SystemExit("Unexpected release dependencies: " + ", ".join(forbidden) + "\n" + linked)
print("Linux release has no native playback or C++ runtime dependencies")
PY
fi
cp README.md THIRD_PARTY.md LICENSE NOTICE "$stage/"
cp -R licenses "$stage/"
mkdir -p "$stage/docs"
cp docs/METADATA.md docs/SELF_UPDATE.md docs/VIDEO.md "$stage/docs/"
if [[ -f assets/demo.gif ]]; then
    mkdir -p "$stage/assets"
    cp assets/demo.gif "$stage/assets/"
fi
tar -C dist -czf "$archive" "$name"
(
    cd dist
    sha256sum "$name.tar.gz" > "$name.tar.gz.sha256"
)
if [[ -n ${UPDATE_BASE_URL:-} ]]; then
    version=$(awk -F'"' '/^const currentVersion = / {print $2; exit}' cmd/spotirip-lossless/update.go)
    checksum=$(sha256sum "$archive" | awk '{print $1}')
    size=$(stat -c %s "$archive")
    url="${UPDATE_BASE_URL%/}/$name.tar.gz"
    platform="$target_os-$target_arch"
    python3 - "$version" "$platform" "$url" "$checksum" "$size" <<'PY'
import json
import sys
from pathlib import Path

version, platform, url, checksum, size = sys.argv[1:]
path = Path("dist/update-manifest.json")
if path.exists():
    manifest = json.loads(path.read_text())
    if manifest.get("version") != version:
        manifest = {"version": version, "platforms": {}}
else:
    manifest = {"version": version, "platforms": {}}
manifest["platforms"][platform] = {"url": url, "sha256": checksum, "size": int(size)}
path.write_text(json.dumps(manifest, indent=2) + "\n")
PY
    printf 'Update-Manifest: dist/update-manifest.json\n'
fi
printf 'Release: %s\n' "$archive"
