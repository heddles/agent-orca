# Agent dispatch: routing spike (Stage 0)

Which decider should pick an agent for a request? This directory holds the
labelled requests, the harness and the measurements behind the answer.

## What was measured

- **Catalogue:** 16 Agents from `config/samples`. `mixed7` is seven clearly
  different agents; `all16` adds the nine software-factory agents, which look
  alike.
- **Cards:** `auto` is the name plus the first 600 characters of the system
  prompt (no setup). `described` is the name plus one hand-written line
  (`descriptions.tsv`), which is what `spec.routing.description` would give.
- **Requests:** `requests.tsv` has 115 requests. 18 are labelled `none`: no
  agent should take them.
- **Runtime:** `llama.cpp` server in Docker, 2 CPUs per server, on Apple Silicon
  (arm64).

## Results

Means of the two catalogue sizes. "Routed" is the share of in-scope requests
sent straight to an agent at a threshold tuned for 90% precision. The rest would
go to the default agent.

| decider | top-1, described / auto | top-3, described / auto | routed, described / auto | p50 latency | memory |
|---|---|---|---|---|---|
| bge-small embeddings (33M) | 57% / 44% | 79% / 73% | 23% / 10% | 5 ms | 0.14 GiB |
| bge-base embeddings | 49% / 55% | 81% / 71% | 31% / 7% | 11 ms | 0.28 GiB |
| nomic-embed v1.5 | 41% / 47% | 69% / 70% | 5% / 6% | 19 ms | 0.20 GiB |
| **Qwen3-Embedding-0.6B** | **73% / 69%** | **88% / 86%** | **51% / 38%** | **55 ms** | 1.4 GiB |
| bge-reranker-base | 63% / 47% | 77% / 68% | 34% / 19% | 190–740 ms | 0.60 GiB |
| bge-small → bge-reranker-base | 61% / 51% | 77% / 67% | 22% / 7% | 90–320 ms | 0.74 GiB |
| ms-marco-MiniLM (ONNX) | 48% / 47% | 72% / 67% | 42% / 15% | 64–240 ms | – |
| jina-reranker-v1-tiny (ONNX) | 47% / 52% | 70% / 72% | 15% / 9% | 54–200 ms | – |
| Qwen3-Reranker-0.6B, all agents | 68% / 68% | 93% / 87% | 20% / 0% | 1.2–3.2 s | 1.6 GiB |
| Qwen3-Embedding → Qwen3-Reranker, top 3 | 68% / 65% | 88% / 86% | 7% / 18% | 0.4–0.8 s | 3.0 GiB |
| Qwen2.5-1.5B yes/no per agent | 61% / – | 93% / – | 14% / – | 4.6 s | 2.2 GiB |
| Qwen2.5-0.5B yes/no per agent | 43% / – | 71% / – | 0% / – | 2.8 s | 0.85 GiB |
| Haiku 5.5, hosted (the `modelSelector` decider) | 99% / 99% | – | 97% / 100% | 0.8 s | – |

Per-catalogue tables, with calibration error and out-of-scope detection, are in
[RESULTS.md](RESULTS.md).

## Findings

1. **A second step does not pay for itself.** No reranker, cascade or small LLM
   beat `Qwen3-Embedding-0.6B` alone on top-1 in the `described` catalogues. The
   Qwen-based ones raised top-3 by up to 5 points, at 7 to 80 times its latency.
2. **The embedder is what matters.** `Qwen3-Embedding-0.6B` is 16 to 25 points
   of top-1 ahead of `bge-small`, and needs about 1.4 GiB.
3. **Small LLMs as routers are weak.** Asking for an option letter was close to
   chance. Yes/no per agent was the best prompt found, and still trails the
   embedder at seconds per request.
4. **A hosted small model is far ahead:** 99% against 73%.
5. **The local decider sends many requests to the default agent:** about half
   with descriptions and more without. Its choices are right when it is sure, and
   it is often not sure.
6. **"No agent fits" needs its own signal.** The softmax probability only
   compares agents with each other. For `bge-small` and the Qwen3 reranker the
   raw top score separates `none` requests clearly better (AUROC 0.88 to 0.98).
   For `Qwen3-Embedding` neither signal wins everywhere (0.68 to 0.88), so the
   dispatcher should use both, tuned per model.
7. **Descriptions help,** but by less than a better embedder does.

## llama.cpp notes for the shipped service

- Set `--cache-ram 0`. The server's prompt cache may grow to 8 GiB by default,
  and it got the reranker OOM-killed here.
- `ms-marco-MiniLM` and `jina-reranker-v1-tiny` GGUF builds do **not** reproduce
  the original models' scores; `bge-reranker-base` does. Check any new model
  against a reference runtime.
- `grammar` was ignored on `/v1/chat/completions` and honoured on `/completion`.

## Limits of this measurement

- The requests were written by one author who knew the agents, and there are only
  115. Treat gaps of a few points as noise.
- Temperature and thresholds are fitted on one half and scored on the other, so
  each "routed" figure rests on few requests and moves a lot.
- The agents are the public samples, not a real tenant's.
- Latency is arm64 under Docker. Measure on x86 before sizing resources.
- The hosted score is probably optimistic for real traffic. It used a forced tool
  call; the operator's one-shot provider path sends no tools, so the decider will
  ask for strict JSON instead.

## Running it

```sh
python3 cards.py                      # rebuild agents.json from config/samples
MODELS=/path/to/gguf ./run.sh         # STEPS="embed rerank qwen3rr llm"
OPENROUTER_API_KEY=... python3 run.py score hosted-haiku-5.5   # about $0.10
python3 run.py report
```

The two ONNX rerankers need `pip install fastembed`:
`python3 run.py score rerank-minilm-onnx --fastembed Xenova/ms-marco-MiniLM-L-6-v2`
and `... rerank-jina-tiny-onnx --fastembed jinaai/jina-reranker-v1-tiny-en`.

`MODELS` holds these files. All base models are Apache-2.0 or MIT.

| file | Hugging Face source |
|---|---|
| embed-bge-small.gguf | CompendiumLabs/bge-small-en-v1.5-gguf (q8_0) |
| embed-bge-base.gguf | CompendiumLabs/bge-base-en-v1.5-gguf (q8_0) |
| embed-nomic.gguf | nomic-ai/nomic-embed-text-v1.5-GGUF (Q8_0) |
| embed-qwen3.gguf | Qwen/Qwen3-Embedding-0.6B-GGUF (Q8_0) |
| rerank-bge-base.gguf | sinjab/bge-reranker-base-Q8_0-GGUF |
| rerank-qwen3.gguf | ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF |
| llm-qwen05.gguf | Qwen/Qwen2.5-0.5B-Instruct-GGUF (q4_k_m) |
| llm-qwen15.gguf | Qwen/Qwen2.5-1.5B-Instruct-GGUF (q4_k_m) |

`llama.cpp` image:
`ghcr.io/ggml-org/llama.cpp@sha256:535b0b2eb8e184668698ebe8204f86ed5e57825965fc5b81b9992c556822e6b9`.
