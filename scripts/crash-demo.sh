#!/usr/bin/env bash
# Kills the worker with SIGKILL in the middle of a run, restarts it, and shows
# that finished nodes were not repeated.
#
# Needs: go, curl, jq, and env DATABASE_URL, TEMPORAL_HOSTPORT, APEX_SECRET_KEY,
# GEMINI_API_KEY. Run from the repository root.
set -euo pipefail

KEY=crash-demo-key-0123456789
API=http://localhost:${APEX_DEMO_PORT:-8790}
DEMO=http://localhost:${DEMO_API_PORT:-8791}
export APEX_ALLOW_PRIVATE_NETWORK=1 APEX_API_KEYS="$KEY:crash-demo-$(date +%s)"
export APEX_ADDR=":${APEX_DEMO_PORT:-8790}" DEMO_ADDR=":${DEMO_API_PORT:-8791}" DEMO_BASE_URL=$DEMO

mkdir -p bin
go build -o bin/apex-engine ./cmd/apex-engine
go build -o bin/demo-api ./cmd/demo-api

bin/demo-api >/dev/null 2>&1 &
DEMO_PID=$!
APEX_ROLE=api bin/apex-engine >/dev/null 2>&1 &
API_PID=$!
APEX_ROLE=worker bin/apex-engine >/dev/null 2>&1 &
WORKER_PID=$!
trap 'kill -9 $DEMO_PID $API_PID ${WORKER_PID:-} ${WORKER2_PID:-} 2>/dev/null || true' EXIT

for _ in $(seq 1 60); do curl -sf "$API/healthz" >/dev/null && curl -sf "$DEMO/hits" >/dev/null && break; sleep 0.5; done

auth=(-H "Authorization: Bearer $KEY" -H 'Content-Type: application/json')
CAT=$(curl -sf "${auth[@]}" "$API/v1/catalogs" -d "{\"name\":\"Demo\",\"source\":{\"type\":\"openapi\",\"url\":\"$DEMO/openapi.json\"}}" | jq -r .id)
curl -sf "${auth[@]}" -X PUT "$API/v1/catalogs/$CAT/credentials" -d '{"headers":{"Authorization":"Bearer demo-token"}}' >/dev/null

node() { # key name seconds dep
  printf '{"key":"%s","type":"api.demo.slowStep","name":"%s","dependsOn":["%s"],"config":{"name":"%s","seconds":"%s"}}' "$1" "$1" "$3" "$1" "$2"
}
DAG="{\"nodes\":[{\"key\":\"t\",\"type\":\"trigger.manual\",\"name\":\"start\",\"dependsOn\":[],\"config\":{}},$(node one 1 t),$(node two 8 one),$(node three 1 two)]}"
RUN=$(curl -sf "${auth[@]}" "$API/v1/runs" -d "{\"dag\":$DAG,\"catalogIds\":[\"$CAT\"]}" | jq -r .id)
echo "run: $RUN"

status() { curl -sf "${auth[@]}" "$API/v1/runs/$RUN"; }
until [ "$(status | jq -r '.nodes[]|select(.key=="two").status')" = running ]; do sleep 0.2; done
echo "node 'two' is running. Sending SIGKILL to the worker."
kill -9 "$WORKER_PID"; WORKER_PID=
status | jq -r '.nodes[]|"  \(.key): \(.status) (attempts \(.attempts))"'

sleep 2
echo "Starting a new worker."
APEX_ROLE=worker bin/apex-engine >/dev/null 2>&1 &
WORKER2_PID=$!

until [ "$(status | jq -r .finishedAt)" != null ]; do sleep 0.5; done
echo "finished: $(status | jq -r .status)"
status | jq -r '.nodes[]|"  \(.key): \(.status) (attempts \(.attempts))"'
echo "calls the upstream API received:"
curl -sf "$DEMO/hits" | jq -S .
