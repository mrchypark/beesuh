#!/bin/sh
set -eu

image=${1:?container image is required}
docker run --rm "$image" -help >/dev/null 2>&1

container=$(docker run -d --rm \
  -p 127.0.0.1::8080 \
  -e BEESUH_API_TOKEN=smoke-test-only \
  -e BEESUH_MODEL_URL=http://127.0.0.1:9/v1 \
  -e BEESUH_MODEL=smoke \
  "$image" -listen 0.0.0.0:8080 -data-dir /data -operator-http)
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT HUP INT TERM

port=$(docker port "$container" 8080/tcp | sed -n '1s/.*://p')
case "$port" in ''|*[!0-9]*) docker logs "$container"; exit 1 ;; esac
url="http://127.0.0.1:$port/v1/status"
attempt=0
until curl --silent --fail --max-time 2 \
  -H 'Authorization: Bearer smoke-test-only' "$url" >/dev/null; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    docker logs "$container"
    exit 1
  fi
  sleep 1
done

status=$(curl --silent --max-time 2 -o /dev/null -w '%{http_code}' "$url")
test "$status" = 401
test "$(docker inspect --format '{{.Config.User}}' "$container")" = beesuh
printf '%s\n' 'Container CLI, Rhiza startup, authenticated status, and authentication rejection passed.'
