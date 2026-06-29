#!/usr/bin/env bash
# start-llama-native.sh — Run llama-server natively on macOS with Metal GPU acceleration.
# Starts two servers:
#   - Chat model (qwen2.5-14b-instruct) on LLAMA_CHAT_PORT (default 11434)
#   - Embedding model (nomic-embed-text-v1.5) on LLAMA_EMBED_PORT (default 18081)
# Use with: skaffold dev -p dev
set -euo pipefail

MODEL_DIR="${LLAMA_MODEL_DIR:-$HOME/llama-models}"

CHAT_MODEL_FILE="qwen2.5-14b-instruct-q4_k_m.gguf"
CHAT_MODEL_URL="https://huggingface.co/bartowski/Qwen2.5-14B-Instruct-GGUF/resolve/main/Qwen2.5-14B-Instruct-Q4_K_M.gguf"
CHAT_PORT="${LLAMA_CHAT_PORT:-11434}"

EMBED_MODEL_FILE="nomic-embed-text-v1.5.Q8_0.gguf"
EMBED_MODEL_URL="https://huggingface.co/nomic-ai/nomic-embed-text-v1.5-GGUF/resolve/main/nomic-embed-text-v1.5.Q8_0.gguf"
EMBED_PORT="${LLAMA_EMBED_PORT:-18081}"

CTX_SIZE="${LLAMA_CTX_SIZE:-16384}"
GPU_LAYERS="${LLAMA_GPU_LAYERS:-99}"

# --- Install llama.cpp if missing ---
if ! command -v llama-server &>/dev/null; then
  echo "Installing llama.cpp via Homebrew..."
  brew install llama.cpp
fi

# --- Download models if missing ---
mkdir -p "$MODEL_DIR"

CHAT_MODEL_PATH="$MODEL_DIR/$CHAT_MODEL_FILE"
if [ ! -f "$CHAT_MODEL_PATH" ]; then
  echo "Downloading chat model to $CHAT_MODEL_PATH..."
  curl -L --progress-bar -o "$CHAT_MODEL_PATH" "$CHAT_MODEL_URL"
else
  echo "Chat model already present: $CHAT_MODEL_PATH"
fi

EMBED_MODEL_PATH="$MODEL_DIR/$EMBED_MODEL_FILE"
if [ ! -f "$EMBED_MODEL_PATH" ]; then
  echo "Downloading embedding model to $EMBED_MODEL_PATH..."
  curl -L --progress-bar -o "$EMBED_MODEL_PATH" "$EMBED_MODEL_URL"
else
  echo "Embedding model already present: $EMBED_MODEL_PATH"
fi

# --- Check port availability ---
for PORT in "$CHAT_PORT" "$EMBED_PORT"; do
  if lsof -iTCP:"$PORT" -sTCP:LISTEN &>/dev/null; then
    echo "Port $PORT is already in use."
    echo "  kill \$(lsof -t -iTCP:$PORT -sTCP:LISTEN) to stop it."
    exit 1
  fi
done

# --- Start embedding server in background ---
echo ""
echo "Starting embedding server (nomic-embed-text-v1.5) on port $EMBED_PORT..."
echo "Logs: /tmp/llama-server-embed.log"
llama-server \
  --model "$EMBED_MODEL_PATH" \
  --host 0.0.0.0 \
  --port "$EMBED_PORT" \
  --embeddings \
  --n-gpu-layers "$GPU_LAYERS" \
  --ctx-size 2048 \
  > /tmp/llama-server-embed.log 2>&1 &
EMBED_PID=$!
echo "Embedding server PID: $EMBED_PID"

# --- Start chat server (foreground) ---
echo ""
echo "Starting chat server (qwen2.5-14b-instruct) on port $CHAT_PORT with Metal GPU (n-gpu-layers=$GPU_LAYERS)..."
echo "Logs: /tmp/llama-server-chat.log"
echo "Stop all: kill $EMBED_PID \$(lsof -t -iTCP:$CHAT_PORT -sTCP:LISTEN)"
echo ""

exec llama-server \
  --model "$CHAT_MODEL_PATH" \
  --host 0.0.0.0 \
  --port "$CHAT_PORT" \
  --ctx-size "$CTX_SIZE" \
  --n-gpu-layers "$GPU_LAYERS" \
  --parallel 2
