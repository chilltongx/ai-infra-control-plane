#!/usr/bin/env bash
set -euo pipefail

base_url=${1:-http://127.0.0.1:18080}
gib=$((1024 * 1024 * 1024))

post_json() {
  curl --fail --silent --show-error \
    -H 'Content-Type: application/json' \
    --data "$2" "$base_url$1"
}

post_json /v1/workers \
  "{\"id\":\"node-browser\",\"name\":\"Windows RTX Lab\",\"adapter\":\"demo.sleep\",\"session_id\":\"browser-session\",\"labels\":{\"os\":\"windows\",\"accelerator\":\"nvidia\"},\"resources\":{\"probe_status\":\"ok\",\"gpus\":[{\"id\":\"GPU-browser-4090\",\"index\":0,\"name\":\"NVIDIA GeForce RTX 4090\",\"vendor\":\"NVIDIA\",\"total_memory_bytes\":$((24 * gib)),\"free_memory_bytes\":$((20 * gib)),\"utilization_percent\":12,\"temperature_celsius\":48}]}}" >/dev/null
worker_id=node-browser

experiment_json=$(post_json /v1/experiments '{"name":"Browser smoke GPU run"}')
experiment_id=$(printf '%s' "$experiment_json" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

post_json "/v1/experiments/$experiment_id/runs" \
  "{\"recipe\":{\"adapter\":\"demo.sleep\",\"command\":[\"2s\"]},\"required_labels\":{\"os\":\"windows\",\"accelerator\":\"nvidia\"},\"resource_requirements\":{\"gpu_count\":1,\"min_free_gpu_memory_bytes\":$((16 * gib))},\"max_attempts\":2}" >/dev/null

curl --fail --silent --show-error -X POST \
  -H 'Content-Type: application/json' \
  --data '{"session_id":"browser-session"}' \
  "$base_url/v1/workers/$worker_id/claim?lease_ttl=5m" >/dev/null
