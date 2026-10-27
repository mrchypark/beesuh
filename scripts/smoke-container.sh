#!/bin/sh
set -eu

image=${1:?container image is required}
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/beesuh-smoke.XXXXXXXX")
containers=''
network=''
cleanup() {
  result=$?
  trap - EXIT HUP INT TERM
  for id in $containers; do
    docker logs "$id" >"$work/$id.log" 2>&1 || true
    if ! docker rm -f "$id" >/dev/null 2>&1; then
      printf 'Could not remove smoke container %s\n' "$id" >&2
      result=1
    fi
    if docker container inspect "$id" >/dev/null 2>&1; then
      printf 'Smoke container remains: %s\n' "$id" >&2
      result=1
    fi
  done
  if [ -n "$network" ]; then
    if ! docker network rm "$network" >/dev/null 2>&1; then
      printf 'Could not remove smoke network %s\n' "$network" >&2
      result=1
    fi
    if docker network inspect "$network" >/dev/null 2>&1; then
      printf 'Smoke network remains: %s\n' "$network" >&2
      result=1
    fi
  fi
  # A failed daemon query cannot establish that cleanup succeeded.
  if ! docker info >/dev/null 2>&1; then result=1; fi
  if [ "$result" -eq 0 ]; then
    rm -rf "$work"
  else
    rm -f "$work/config/key.pem" "$work/config/client-secret" "$work/bee-config/client-secret"
    printf 'Smoke failed; logs preserved at %s\n' "$work" >&2
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM

# The fixture is a nested module, outside the root module's test/vet scope.
(cd "$root/scripts/smoke-container-src" && go test ./... && go vet ./...)

# Pin all runs to the inspected image, including the issuer helper.
image=$(docker image inspect --format '{{.Id}}' "$image")
arch=$(docker image inspect --format '{{.Architecture}}' "$image")
mkdir "$work/config"
(cd "$root/scripts/smoke-container-src" && go run . init "$work/config")
(cd "$root/scripts/smoke-container-src" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$work/issuer" .)
chmod 755 "$work/config"
chmod 644 "$work/config/"*
mkdir "$work/bee-config"
chmod 755 "$work/bee-config"
for file in ca.pem providers.json service.json client-secret; do
  cp "$work/config/$file" "$work/bee-config/$file"
done

network=$(docker network create --internal "beesuh-smoke-$(basename "$work")")

run_check() {
  check=$1
  shift
  id=$(docker create --network "$network" "$image" "$@")
  containers="$id $containers"
  docker start -a "$id" >"$work/$check.log" 2>&1 || true
  test "$(docker inspect --format '{{.State.ExitCode}}' "$id")" -ne 0
}
help=$(docker create --network "$network" "$image" -help)
containers="$help $containers"
docker start -a "$help" >"$work/help.log" 2>&1
test "$(docker inspect --format '{{.State.ExitCode}}' "$help")" -eq 0
run_check no-service-config -listen 0.0.0.0:8080
grep -q -- '-listen requires -service-config' "$work/no-service-config.log"
run_check removed-flag -operator-http
grep -q -- 'flag provided but not defined: -operator-http' "$work/removed-flag.log"

fixture=$(docker create --network "$network" --network-alias smoke-issuer --entrypoint /tmp/smoke-issuer "$image")
containers="$fixture $containers"
docker cp "$work/issuer" "$fixture:/tmp/smoke-issuer"
docker cp "$work/config" "$fixture:/config"
docker start "$fixture" >/dev/null

container=$(docker create --network "$network" --network-alias smoke-beesuh \
  -e SSL_CERT_FILE=/config/ca.pem "$image" \
  -listen 0.0.0.0:8080 -backend native -data-dir /data \
  -providers /config/providers.json -service-config /config/service.json)
containers="$container $containers"
docker cp "$work/bee-config" "$container:/config"
docker start "$container" >/dev/null
test "$(docker inspect --format '{{.Config.User}}' "$container")" = beesuh
test "$(docker exec "$container" id -u)" -ne 0
docker exec "$fixture" /tmp/smoke-issuer probe http://smoke-beesuh:8080/v1/providers \
  >"$work/probe.log" 2>&1 || { cat "$work/probe.log" >&2; exit 1; }
docker logs "$fixture" >"$work/issuer.log" 2>&1
grep -q 'validated introspection' "$work/issuer.log"
grep -q 'validated userinfo' "$work/issuer.log"
printf '%s\n' 'Container CLI, nonroot Rhiza startup, TLS-authenticated providers, 401 rejection, and invalid flags passed.'
