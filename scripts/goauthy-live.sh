#!/bin/sh
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cache=${XDG_CACHE_HOME:-$HOME/.cache}/beesuh-goauthy-live
mkdir -p "$cache"
(cd "$root/scripts/goauthy-live-src" && go build -o "$cache/goauthy-live" .)
exec "$cache/goauthy-live" -beesuh "$root" -goauthy "$root/../goauthy" "$@"
