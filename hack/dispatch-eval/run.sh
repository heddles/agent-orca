#!/usr/bin/env bash
# Runs the routing spike. MODELS is a directory holding the GGUF files named in
# README.md. Every llama.cpp server is limited to 2 CPUs.
set -euo pipefail
cd "$(dirname "$0")"
: "${MODELS:?set MODELS to the directory with the GGUF files}"
IMAGE=${IMAGE:-ghcr.io/ggml-org/llama.cpp@sha256:535b0b2eb8e184668698ebe8204f86ed5e57825965fc5b81b9992c556822e6b9}

serve() { # name port model flags...
  local name=$1 port=$2 model=$3; shift 3
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --cpus=2 -v "$MODELS:/m:ro" -p "127.0.0.1:$port:8080" "$IMAGE" -m "/m/$model" -t 2 --cache-ram 0 "$@" >/dev/null
  for _ in $(seq 1 240); do curl -sf "localhost:$port/health" >/dev/null && return; perl -e 'select undef,undef,undef,0.5'; done
  docker logs "$name" | tail -20; return 1
}
mem() { echo "MEM $1 $(docker stats --no-stream --format '{{.MemUsage}}' "$2")"; }
trap 'docker rm -f spike-a spike-b >/dev/null 2>&1 || true' EXIT

embed() { # label file family pooling
  serve spike-a 18080 "$2" --embedding --pooling "$4" -c 2048 -b 2048 -ub 2048
  python3 run.py score "embed-$1" --family "$3"; mem "embed-$1" spike-a
  python3 run.py score "embed-$1-split" --family "$3" --split
}

for step in ${STEPS:-embed rerank llm}; do
  case $step in
  embed)
    embed bge-small embed-bge-small.gguf bge cls
    embed bge-base embed-bge-base.gguf bge cls
    embed nomic embed-nomic.gguf nomic mean
    embed qwen3 embed-qwen3.gguf qwen3 last ;;
  rerank)
    serve spike-a 18080 embed-bge-small.gguf --embedding --pooling cls
    serve spike-b 18081 rerank-bge-base.gguf --reranking -b 1024 -ub 1024
    python3 run.py score rerank-bge-base --url http://127.0.0.1:18081; mem rerank-bge-base spike-b
    python3 run.py score cascade-bge; docker rm -f spike-b >/dev/null ;;
  qwen3rr)
    serve spike-a 18080 embed-qwen3.gguf --embedding --pooling last -c 2048 -b 2048 -ub 2048
    serve spike-b 18081 rerank-qwen3.gguf -c 4096 -np 1
    for k in 3 5; do python3 run.py score "cascade-qwen3rr-top$k" --family qwen3 --rerank-url http://127.0.0.1:18081 -k "$k"; done
    mem qwen3rr spike-b
    python3 run.py score qwen3rr-all --url http://127.0.0.1:18081 ;;
  llm)
    for q in 05 15; do
      serve spike-a 18080 "llm-qwen$q.gguf" -c 4096 -np 1
      python3 run.py score "llm-yesno-qwen$q" --only described; mem "llm-qwen$q" spike-a
    done ;;
  esac
done
