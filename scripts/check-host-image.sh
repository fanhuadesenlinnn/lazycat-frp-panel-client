#!/usr/bin/env bash
# Exercise the real Host backend, shared Unix Socket, app gateway and ingress auth.
set -euo pipefail
IMAGE="${1:?image reference required}"
work="$(mktemp -d)"
host_pid=""
backend="frpp-host-check-backend"
gateway="frpp-host-check-gateway"
cleanup() {
  docker rm -f "$gateway" "$backend" >/dev/null 2>&1 || true
  [ -z "$host_pid" ] || kill "$host_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

python3 - "$work/port" <<'PY' >"$work/host-http.log" 2>&1 &
import http.server, sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'host-loopback-ok')
server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
with open(sys.argv[1], 'w') as output:
    output.write(str(server.server_port))
server.serve_forever()
PY
host_pid=$!
for attempt in $(seq 1 20); do
  [ ! -s "$work/port" ] || break
  sleep 1
done
host_url="http://127.0.0.1:$(cat "$work/port")"

for arch in amd64 arm64; do
  mkdir -p "$work/$arch"
  docker run -d --name "$backend" --platform "linux/$arch" --network host \
    --mount "type=bind,source=$work/$arch,target=/lzcapp/var/frp-panel-client" \
    -e LZCFRPP_ROLE=backend -e LZCFRPP_TRUST_INGRESS=1 \
    "$IMAGE" /app/start.sh --probe-interval=1s >/dev/null
  docker run -d --name "$gateway" --platform "linux/$arch" \
    --mount "type=bind,source=$work/$arch,target=/lzcapp/var/frp-panel-client" \
    -e LZCFRPP_ROLE=gateway "$IMAGE" >/dev/null
  ready=0
  for attempt in $(seq 1 30); do
    if docker exec "$gateway" wget -q -O /dev/null http://127.0.0.1:18080/healthz; then ready=1; break; fi
    sleep 1
  done
  if [ "$ready" != 1 ]; then docker logs "$backend"; docker logs "$gateway"; exit 1; fi
  [ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$backend")" = host ]
  # A normal app entry cannot read data without a Lazycat identity.
  if docker exec "$gateway" wget -q -O /dev/null http://127.0.0.1:18080/api/status; then
    echo 'Unauthenticated status request was accepted' >&2; exit 1
  fi
  docker exec "$gateway" wget -q -O /dev/null \
    --header='X-HC-User-ID: ci-admin' --header='X-HC-User-Role: ADMIN' \
    --header='Origin: http://127.0.0.1:18080' \
    --header='X-Requested-With: lzc-frpp-client' --header='Content-Type: application/json' \
    --post-data="{\"api_url\":\"$host_url\"}" http://127.0.0.1:18080/api/config
  reached=0
  for attempt in $(seq 1 20); do
    docker exec "$gateway" wget -q -O - \
      --header='X-HC-User-ID: ci-admin' --header='X-HC-User-Role: ADMIN' \
      http://127.0.0.1:18080/api/status >"$work/status.json"
    if node -e 'const s=require(process.argv[1]); const p=s.remote.probe; process.exit(p.tcp==="ok"&&p.http_status===200&&p.target===process.argv[2]?0:1)' "$work/status.json" "$host_url"; then reached=1; break; fi
    sleep 1
  done
  [ "$reached" = 1 ] || { cat "$work/status.json"; exit 1; }
  docker exec "$gateway" wget -q -O - http://127.0.0.1:18080/ >"$work/index.html"
  grep -q 'frp-panel 客户端管理' "$work/index.html"
  echo "$arch: host loopback, Unix Socket, gateway authentication and app title verified"
  docker rm -f "$gateway" "$backend" >/dev/null
done
