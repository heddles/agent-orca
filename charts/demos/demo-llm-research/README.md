# LLM Research Agent Demo

Autonomous research agent that builds a comprehensive knowledge base about LLM architecture,
layer interpretation, and model capabilities by scouring public research sources.

## What This Demo Shows

A single research agent with:

1. **Web Research Tools** — Fetch web pages, search via DuckDuckGo, extract PDFs, and query arXiv
2. **25Gi Knowledge Base** — Local Ollama embeddings for RAG indexing of research documents
3. **Periodic Ingestion** — Automatically fetches new arXiv papers every 6 hours via CronJobs
4. **Poolside Laguna Models** — Uses laguna-m and laguna-xs series for reasoning tasks

## Evaluation Suite (Laguna XS.2 vs M.1 Code Generation)

The demo includes an autonomous evaluation system that benchmarks code generation capabilities:

### Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                   llm-research Agent                              │
│  Can spawn the evaluation-coordinator-agent for benchmarking       │
└─────────────────────────────────────────────────────────────────┘
                                │ _spawn
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│              evaluation-coordinator Agent                         │
│  Orchestrates 8 evaluation agents (4 languages × 2 models)        │
│  Stores results in evaluation-metrics-kb                         │
└─────────────────────────────────────────────────────────────────┘
                                │ spawns
                    ┌───────────┼───────────┬────────────┐
                    ▼           ▼           ▼            ▼
         ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
         │ eval-py-xs2  │ │ eval-go-xs2  │ │ eval-js-xs2  │ │ eval-f90-xs2 │
         └──────────────┘ └──────────────┘ └──────────────┘ └──────────────┘
                    │           │           │            │
                    ▼           ▼           ▼            ▼
         ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
         │ eval-py-m1   │ │ eval-go-m1   │ │ eval-js-m1   │ │ eval-f90-m1  │
         └──────────────┘ └──────────────┘ └──────────────┘ └──────────────┘
```

### Evaluation Metrics

Each evaluation agent reports the following metrics (based on HumanEval/MBPP/ALFWorld standards):

| Metric | Description | Formula | Source |
|--------|-------------|---------|--------|
| **pass@k** | Probability that at least one of k samples passes all tests | 1 - (n-c)!/(n-k)! × k!/(n)! | [HumanEval paper](https://arxiv.org/abs/2107.03374) |
| **pass@1** | Success rate on first attempt only | 1.0 if first attempt succeeds, 0.0 otherwise | HumanEval baseline |
| **first_success_attempt** | Which attempt first succeeded (1-5) or null | - | Iterative refinement tracking |
| **total_tokens** | Cumulative prompt + generated tokens | Σ(tokens per attempt) | Cost/efficiency tracking |
| **duration_ms** | Wall-clock execution time | time.time() delta | Performance measurement |
| **correctness** | Overall result: "pass" or "fail" | - | Binary success indicator |

**Why these metrics?**

- **pass@k**: Standard metric from OpenAI's HumanEval benchmark. Measures the probability of generating a correct solution within k attempts, accounting for sampling variability.
- **pass@1**: Baseline single-attempt success rate. Important for comparing against traditional static benchmark evaluations.
- **first_success_attempt**: Measures how quickly each model converges. Lower is better (shows faster debugging/refinement).
- **total_tokens**: Efficiency metric. Models that need fewer tokens to solve problems are more cost-effective.
- **duration_ms**: Real-world performance including execution overhead. The iterative loop can be slow for complex tasks.

The 5-attempt limit (pass@5) was chosen as a reasonable tradeoff between giving the model enough chances to succeed while keeping evaluation time bounded for autonomous operation.

### Evaluation Tasks

| Language | Task | Model |
|----------|------|-------|
| Python | Flask REST API with todo CRUD | XS.2, M.1 |
| Go | Concurrent worker pool with graceful shutdown | XS.2, M.1 |
| Fortran | Numerical integration using Simpson's rule | XS.2, M.1 |
| JavaScript | JSON schema parsing and validation module | XS.2, M.1 |

Each task runs up to 5 iterative refinement cycles: generate → execute → check → fix.

### eval-runtime Docker Image

The evaluation agents use a custom multi-language runtime:

```dockerfile
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    nodejs npm golang-go gfortran && rm -rf /var/lib/apt/lists/*
```

No additional Python packages needed — `urllib.request` from stdlib handles HTTP calls to the model-router.

Build the image locally:
```bash
docker build -f charts/demos/demo-llm-research/Dockerfile.eval -t ghcr.io/agentorc/eval-runtime:latest .
```

Push to GitHub Container Registry:
```bash
# Login to GHCR (requires ghcr.io write access)
echo $CR_PAT | docker login ghcr.io -u USERNAME --password-stdin

# Push the image
docker push ghcr.io/agentorc/eval-runtime:latest
```

For local kind clusters, the skaffold profile automatically loads the image.

### Usage

Run evaluations:
```bash
# Trigger from llm-research agent
curl -X POST http://agent-orc-ui/api/deployments/agent-orc-system/llm-research/execute \
  -d '{"input": "Run the full evaluation suite comparing Laguna XS.2 and M.1 models"}'

# Or call coordinator directly
curl -X POST http://agent-orc-ui/api/deployments/agent-orc-system/evaluation-coordinator/execute \
  -d '{"input": "Run full evaluation suite"}'
```

Query results:
```bash
# Search evaluation results
curl -X POST http://agent-orc-ui/api/deployments/agent-orc-system/llm-research/execute \
  -d '{"input": "Show me evaluation results for python from the evaluation-metrics-kb"}'
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                     llm-research Agent                           │
│  Model: poolside-laguna-m-ctx256k / poolside-laguna-xs-polaris    │
│  Knowledge Base: llm-research-kb (25Gi Qdrant)                  │
└─────────────────────────────────────────────────────────────────┘
          │ tools                     │ ingest
          ▼                           ▼
┌─────────────────────────────────────────────────────────────────┐
│                   research-tools MCP Server                       │
│  - web-fetch    (URL → text)                                     │
│  - web-search   (DuckDuckGo)                                     │
│  - pdf-extract  (PDF → text)                                     │
│  - arxiv-search (arXiv API)                                      │
│  - arxiv-fetch  (Paper abstract + metadata)                      │
│  - semaphore    (Topic completion signal)                        │
└─────────────────────────────────────────────────────────────────┘
          │
          ▼
┌─────────────────────────────────────────────────────────────────┐
│              Periodic KB Ingestion (CronJob)                      │
│  Every 6 hours → arxiv-search → arxiv-fetch → ingest             │
└─────────────────────────────────────────────────────────────────┘
```

## Prerequisites

1. **agent-orc operator** deployed in the cluster
2. **model-providers** chart deployed with:
   - Poolside API key (poolside-api-key secret)
   - Ollama embedding deployed (ollama-nomic-embed-text model provider)

Deploy prerequisites:
```bash
# Using skaffold
skaffold dev -p dev  # Deploys operator + model-providers with poolside and ollama

# Or manually
helm install model-providers charts/model-providers \
  --set providerSecrets[3].enabled=true \
  --set providerSecrets[3].apiKey=$POOLSIDE_API_KEY \
  --set providerSecrets[9].enabled=true
```

## Deployment

```bash
skaffold run -p demo-llm-research
```

Or using helm directly:
```bash
helm install demo-llm-research charts/demos/demo-llm-research -n agent-orc-system
```

## Usage

Query the agent via the chat deployment:

```bash
# Example queries
curl -X POST http://agent-orc-ui/api/deployments/agent-orc-system/llm-research/execute \
  -d '{"input": "Research transformer attention mechanisms and summarize key findings"}'

curl -X POST http://agent-orc-ui/api/deployments/agent-orc-system/llm-research/execute \
  -d '{"input": "Find recent arXiv papers on mechanistic interpretability of large language models"}'

curl -X POST http://agent-orc-ui/api/deployments/agent-orc-system/llm-research/execute \
  -d '{"input": "What do we know about how LLMs represent concepts across layers?"}'
```

## Research Process

The agent follows this workflow:

1. **Query Knowledge Base** — Check `_rag_search` for existing relevant information
2. **Identify Gaps** — Determine what research topics need deeper investigation
3. **Source Discovery** — Use `arxiv-search` and `web-search` to find relevant papers
4. **Content Extraction** — Fetch abstracts and content via `arxiv-fetch` and `web-fetch`
5. **Knowledge Ingestion** — Store findings via `_rag_ingest` with source metadata
6. **Synthesis** — Call `semaphore` when research milestones are reached
7. **Iterate** — Continue expanding to related topics

## Key Research Topics

The agent is configured to research:

- Transformer architecture fundamentals and variants
- Attention mechanism analysis and visualization techniques
- Layer-wise interpretability (LLM dissect, activation patching)
- Chain-of-thought and reasoning emergence
- Embedding spaces and semantic geometry
- Model scaling laws and capability thresholds
- Mechanistic interpretability research

## Periodic arXiv Ingestion

The KnowledgeBase is configured with `syncIntervalSeconds: 21600` (6 hours). The operator
creates a CronJob that:

1. Calls `arxiv-search` with the query: `(cat:cs.LG OR cat:cs.AI OR cat:cs.CL) AND (LLM OR transformer OR attention OR interpretability)`
2. Fetches up to 20 newest papers matching the criteria (reduced for reliable incremental ingestion)
3. Ingests abstracts and metadata into the vector store (one document at a time to avoid timeouts)

## Knowledge Base Size

- **25Gi** allocated for Qdrant vector store
- Approximately 10,000-50,000 research documents depending on chunk size
- Embeddings powered by Ollama's nomic-embed-text (local, no cost)

## Cleanup

```bash
skaffold delete -p demo-llm-research
# Or
helm uninstall demo-llm-research -n agent-orc-system
```

## Troubleshooting

- Check MCP server logs: `kubectl logs -l app=research-tools-server -n agent-orc-system`
- Check KB sync jobs: `kubectl get jobs -n agent-orc-system -l agentorc.io/knowledgebase=llm-research-kb`
- Verify tools: `kubectl get tools -n agent-orc-system -l agentorc.io/mcpserver=research-tools`

### Evaluation-Specific Troubleshooting

- Check evaluation coordinator: `kubectl logs -l agentorc.io/agent=evaluation-coordinator-agent -n agent-orc-system`
- Check individual evaluation agents: `kubectl get pods -n agent-orc-system -l agentorc.io/agent=eval-python-xs2-agent`
- Query evaluation metrics: search `evaluation-metrics-kb` via `_rag_search` tool