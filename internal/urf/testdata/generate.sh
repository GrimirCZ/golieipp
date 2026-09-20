#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")"/../../.. && pwd)
out="$root/internal/urf/testdata"

fixture_go_cache=${FIXTURE_GO_CACHE:-/tmp/golieipp-urf-fixtures-cache}
cd "$root"
GOCACHE="$fixture_go_cache" go run ./internal/urf/testdata/cmd/generate -out "$out"

if ! command -v cups-config >/dev/null 2>&1; then
  echo "generated URF fixtures; CUPS references skipped (cups-config missing)" >&2
  exit 0
fi

cups_dir=${CUPS_FILTER_DIR:-/usr/libexec/cups/filter}
if [ ! -x "$cups_dir/rastertopwg" ]; then
  echo "generated URF fixtures; CUPS references skipped (rastertopwg missing)" >&2
  exit 0
fi

python=${PYTHON:-python3}
oracle="$out/cmd/qualify/cups_oracle.py"
if ! command -v "$python" >/dev/null 2>&1 ||
   ! PYTHONDONTWRITEBYTECODE=1 "$python" "$oracle" --probe >/dev/null 2>&1; then
  echo "generated URF fixtures; CUPS references skipped (Python/libcups missing)" >&2
  exit 0
fi

sha256_file() {
  file=$1
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  else
    echo "no SHA-256 utility available" >&2
    return 1
  fi
}

mkdir -p "$out/reference"
# These were produced by the former rastertotiff/ImageMagick path.  Direct
# libcups decoding below is the authoritative reference, so do not retain
# stale optional RGBA artifacts after regeneration.
rm -f "$out"/pixels/*.rgba "$out"/reference/*.rgba.sha256 "$out"/reference/*.tif
for urf in "$out"/fixtures/*.urf; do
  name=$(basename "$urf" .urf)
  "$cups_dir/rastertopwg" 1 fixture title 1 '' "$urf" >"$out/reference/$name.pwg" 2>"$out/reference/$name.cups.log"
  PYTHONDONTWRITEBYTECODE=1 "$python" "$oracle" "$out/reference/$name.pwg" >"$out/reference/$name.cups.json"
  sha256_file "$out/reference/$name.pwg" >"$out/reference/$name.pwg.sha256"
done

echo "generated URF fixtures and CUPS reference outputs using $(cups-config --version 2>/dev/null || true)" >&2
