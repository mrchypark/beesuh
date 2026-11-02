#!/bin/sh
set -eu
test "${CI:-}" = true || { echo 'Functional container smoke requires an isolated CI runner' >&2; exit 1; }

image=${1:?container image is required}
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/beesuh-smoke.XXXXXXXX")
containers=''
network=''
volume=''
volumes=''
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
  for volume in $volumes; do
    if ! docker volume rm "$volume" >"$work/volume-cleanup.log" 2>&1; then
      printf 'Could not remove smoke volume %s (see volume-cleanup.log)\n' "$volume" >&2
      result=1
    fi
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      printf 'Smoke volume remains: %s\n' "$volume" >&2
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
    rm -f "$work/config/key.pem" "$work/config/client-secret" "$work/config/grant-app-key" "$work/bee-config/client-secret" "$work/bee-config/grant-app-key" "$work/unexpected-private-key"
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
for file in ca.pem providers.json service.json client-secret shared-service.json partial-service.json grant-app-key; do
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

# Mount the same retained data in init and main, as in the release deployment.
volume="beesuh-smoke-$(basename "$work")"
volumes="$volume $volumes"
printf '%s\n' "$volume" >"$work/volume-name"
docker volume create "$volume" >"$work/volume-create.log"
init=$(docker create --network "$network" --mount "type=volume,src=$volume,dst=/data" \
  "$image" -storage-init -data-dir /data/storage)
containers="$init $containers"
docker cp "$init:/data" "$work/empty-data"
test -z "$(ls -A "$work/empty-data")"
docker start -a "$init" >"$work/storage-init.log" 2>&1
test "$(docker inspect --format '{{.State.ExitCode}}' "$init")" -eq 0
docker cp "$init:/data/storage" "$work/initialized-storage"
test -n "$(ls -A "$work/initialized-storage")"
printf '%s\n' 'retained storage sentinel' >"$work/sentinel"
docker cp "$work/sentinel" "$init:/data/storage/sentinel"
docker start -a "$init" >"$work/storage-init-repeat.log" 2>&1
test "$(docker inspect --format '{{.State.ExitCode}}' "$init")" -eq 0
docker cp "$init:/data/storage/sentinel" "$work/sentinel-after"
cmp "$work/sentinel" "$work/sentinel-after"
run_check storage-init-listen -storage-init -data-dir /data/must-not-exist -listen 0.0.0.0:8080
grep -q -- '-storage-init cannot be combined with -listen' "$work/storage-init-listen.log"
docker cp "$id:/data" "$work/rejected-data"
test -z "$(ls -A "$work/rejected-data")"
printf '%s\n' 'Standalone storage init on an owned volume, repeated init with retained sentinel, and incompatible flags before data creation passed.'

fixture=$(docker create --network "$network" --network-alias smoke-issuer --entrypoint /tmp/smoke-issuer "$image")
containers="$fixture $containers"
docker cp "$work/issuer" "$fixture:/tmp/smoke-issuer"
docker cp "$work/config" "$fixture:/config"
docker start "$fixture" >/dev/null

container=$(docker create --network "$network" --network-alias smoke-beesuh \
  --mount "type=volume,src=$volume,dst=/data" \
  -e SSL_CERT_FILE=/config/ca.pem "$image" \
  -listen 0.0.0.0:8080 -backend native -data-dir /data/storage \
  -providers /config/providers.json -service-config /config/service.json)
containers="$container $containers"
docker cp "$work/bee-config" "$container:/config"
docker start "$container" >/dev/null
test "$(docker inspect --format '{{.Config.User}}' "$container")" = beesuh
test "$(docker exec "$container" id -u)" -ne 0
if docker cp "$container:/config/key.pem" "$work/unexpected-private-key" >"$work/private-key-check.log" 2>&1; then
  printf '%s\n' 'Issuer private key reached the application container' >&2
  exit 1
fi
docker exec "$fixture" /tmp/smoke-issuer probe http://smoke-beesuh:8080/v1/providers \
  >"$work/probe.log" 2>&1 || { cat "$work/probe.log" >&2; exit 1; }
docker logs "$fixture" >"$work/issuer.log" 2>&1
grep -q 'validated introspection' "$work/issuer.log"
grep -q 'validated userinfo' "$work/issuer.log"
docker cp "$container:/data/storage/sentinel" "$work/sentinel-main"
cmp "$work/sentinel" "$work/sentinel-main"
printf '%s\n' 'Container CLI, nonroot Rhiza startup, TLS-authenticated empty providers before connection selection, 401 rejection, and invalid flags passed.'

docker exec "$fixture" /tmp/smoke-issuer exercise http://smoke-beesuh:8080 \
  >"$work/exercise.log" 2>&1 || { cat "$work/exercise.log" >&2; exit 1; }
# Restart the exact application container on its original initialized volume.
docker restart "$container" >"$work/restart.log"
docker exec "$fixture" /tmp/smoke-issuer resume http://smoke-beesuh:8080 \
  >"$work/resume.log" 2>&1 || { cat "$work/resume.log" >&2; exit 1; }
docker stop "$container" >"$work/stop.log"
docker cp "$container:/data/storage" "$work/final-storage"
docker cp "$container:/data/storage/sentinel" "$work/sentinel-restarted"
cmp "$work/sentinel" "$work/sentinel-restarted"
docker cp "$container:/tmp" "$work/personal-tmp"
docker cp "$container:/app" "$work/personal-app"

# The same real application image, separate owned data and independent fake PDP.
start_shared() {
  config=$1
  alias=$2
  shared_volume="beesuh-smoke-$(basename "$work")-$alias"
  volumes="$shared_volume $volumes"
  docker volume create "$shared_volume" >"$work/$alias-volume.log"
  shared_init=$(docker create --network "$network" --mount "type=volume,src=$shared_volume,dst=/data" \
    "$image" -storage-init -data-dir /data/storage)
  containers="$shared_init $containers"
  docker start -a "$shared_init" >"$work/$alias-init.log" 2>&1
  test "$(docker inspect --format '{{.State.ExitCode}}' "$shared_init")" -eq 0
  shared_container=$(docker create --network "$network" --network-alias "$alias" \
    --mount "type=volume,src=$shared_volume,dst=/data" -e SSL_CERT_FILE=/config/ca.pem "$image" \
    -listen 0.0.0.0:8080 -backend native -data-dir /data/storage \
    -providers /config/providers.json -service-config "/config/$config")
  containers="$shared_container $containers"
  docker cp "$work/bee-config" "$shared_container:/config"
  docker start "$shared_container" >/dev/null
}
start_shared shared-service.json smoke-shared
docker exec "$fixture" /tmp/smoke-issuer shared-exercise http://smoke-shared:8080 \
  >"$work/shared-exercise.log" 2>&1 || { cat "$work/shared-exercise.log" >&2; exit 1; }
docker restart "$shared_container" >"$work/shared-restart.log"
docker exec "$fixture" /tmp/smoke-issuer shared-resume http://smoke-shared:8080 \
  >"$work/shared-resume.log" 2>&1 || { cat "$work/shared-resume.log" >&2; exit 1; }
docker stop "$shared_container" >"$work/shared-stop.log"
test "$(docker inspect --format '{{.State.ExitCode}}' "$shared_container")" -eq 0
docker cp "$shared_container:/data/storage" "$work/shared-storage"
docker cp "$shared_container:/tmp" "$work/shared-tmp"
docker cp "$shared_container:/app" "$work/shared-app"
start_shared partial-service.json smoke-partial
docker exec "$fixture" /tmp/smoke-issuer shared-partial http://smoke-partial:8080 \
  >"$work/shared-partial.log" 2>&1 || { cat "$work/shared-partial.log" >&2; exit 1; }
docker stop "$shared_container" >"$work/partial-stop.log"
test "$(docker inspect --format '{{.State.ExitCode}}' "$shared_container")" -eq 0
docker cp "$shared_container:/data/storage" "$work/partial-storage"
docker cp "$shared_container:/tmp" "$work/partial-tmp"
docker cp "$shared_container:/app" "$work/partial-app"
mkdir "$work/logs"
for id in $containers; do
  docker logs "$id" >"$work/logs/$id.log" 2>&1
  docker inspect --format '{{json .Config.Env}}' "$id" >"$work/logs/$id-env.log"
done
(cd "$root/scripts/smoke-container-src" && go run . scan \
  "$work/final-storage" "$work/personal-tmp" "$work/personal-app" \
  "$work/shared-storage" "$work/shared-tmp" "$work/shared-app" \
  "$work/partial-storage" "$work/partial-tmp" "$work/partial-app" "$work/logs")
printf '%s\n' 'Personal connection selection, fake credential delivery, minimax-m3.1-flash run/read/replay, active cancellation, durable intent after restart, and storage/log credential absence passed.'
printf '%s\n' 'Shared-workspace fixture-contract flow passed: original Bearer, independently checked PDP digests/bindings, Goauthy consent and API-key credential_delivery, openai-compatible opencode minimax-m3.1-flash POST, fail-closed negatives, restart and runtime/log secret absence. Not actual Grant/Exposure or live-provider qualification.'
