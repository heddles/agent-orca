# Vector Database Selection: Why Qdrant

**Status**: Accepted
**Date**: 2026-03-26

---

## Context

agent-orc's KnowledgeBase feature needs a vector database to store embeddings and serve nearest-neighbor queries for the RAG pipeline. The vector store is deployed **per-KnowledgeBase** as a dedicated StatefulSet managed by the operator — not as a shared cluster service. This deployment model heavily influences the selection criteria.

Each KnowledgeBase gets its own instance (`kb-qdrant-<name>`) with a 10Gi PVC, 256Mi–512Mi memory, and 100m CPU. The operator creates collections, upserts embeddings via gRPC, and queries via cosine similarity. The entire lifecycle — deploy, create collection, ingest, search, teardown — is managed declaratively through the KnowledgeBase CRD.

---

## Requirements

1. **Embeddable per-tenant deployment** — one instance per KnowledgeBase, managed as a StatefulSet by the operator. Must start fast and run with minimal resources.
2. **gRPC API** — the operator and ingestion pipeline are written in Go. A native gRPC client avoids HTTP overhead and gives us streaming/batching for free.
3. **Low operational footprint** — single-binary, single-replica, no external dependencies (no etcd, no ZooKeeper, no separate metadata store).
4. **Rich payload storage** — store chunk text, doc IDs, chunk indices, and arbitrary user metadata alongside vectors. Query payloads back with search results without a second lookup.
5. **Deterministic point IDs** — support user-supplied string IDs (`sha256(docID:chunkIndex)`) for idempotent re-ingestion.
6. **Cosine distance** — standard for text embedding models (OpenAI, nomic-embed-text, etc.).
7. **Open source with a permissive license** — Apache 2.0 or equivalent. No SSPL, no BSL, no proprietary requirements.

---

## Options Evaluated

### Qdrant

| Criterion | Assessment |
|---|---|
| Language / binary | Rust. Single static binary, ~50MB image. Starts in <2s. |
| Deployment model | Runs standalone with no dependencies. One process, one data directory. Perfect for per-KB StatefulSets. |
| API | gRPC (primary) + REST. Official Go gRPC client (`github.com/qdrant/go-client`). |
| Payload filtering | First-class structured payloads with nested objects, typed fields, and filterable queries. |
| Point IDs | Supports user-supplied string or integer IDs natively. |
| Memory footprint | Runs comfortably at 256Mi for small-to-medium collections (tens of thousands of points). |
| Quantization | Scalar, product, and binary quantization available when needed. |
| License | Apache 2.0. |
| Maturity | Production-grade. Used by large-scale deployments. Active development, frequent releases. |

### Milvus

| Criterion | Assessment |
|---|---|
| Language / binary | Go + C++. Requires etcd + MinIO + multiple microservices for distributed mode. Standalone mode still starts etcd and MinIO internally. |
| Deployment model | Designed for large-scale shared clusters, not per-tenant lightweight instances. Standalone mode pulls in ~1.5GB of dependencies and takes 15–30s to become ready. |
| API | gRPC + REST. Official Go SDK available. |
| Payload filtering | Supports scalar filtering on fields, but payload model is less flexible than Qdrant's nested structures. |
| Point IDs | Supports user-supplied IDs. |
| Memory footprint | Standalone mode: 512Mi–1Gi minimum due to embedded etcd + MinIO. Unsuitable for running dozens of lightweight instances. |
| License | Apache 2.0. |

**Why not**: Milvus is built for billion-vector scale with a distributed architecture. Running one Milvus instance per KnowledgeBase would waste 3–5x the resources for our workload sizes, and the startup time degrades the reconciliation loop. The etcd/MinIO dependencies add failure modes the operator would need to manage. Milvus is the right choice if you need a shared multi-tenant cluster at massive scale — that's not our deployment model.

### Weaviate

| Criterion | Assessment |
|---|---|
| Language / binary | Go. Single binary, but heavier than Qdrant (~200MB image, higher baseline memory). |
| Deployment model | Can run standalone. Designed around a module system (vectorizers, readers, generative) that adds complexity we don't need. |
| API | GraphQL (primary) + REST. No native gRPC. Go client uses GraphQL under the hood. |
| Payload filtering | GraphQL-based filtering — powerful but verbose for programmatic use from Go. |
| Point IDs | UUID-based by default. User-supplied deterministic IDs require explicit configuration. |
| Memory footprint | 400Mi–512Mi baseline. The module system loads components we don't use. |
| License | BSD-3-Clause. |

**Why not**: Weaviate's value proposition is its module ecosystem — built-in vectorizers, rerankers, and generative modules that eliminate the need for separate embedding services. We already have a vendor-agnostic embedding pipeline (OpenAI-compatible `/v1/embeddings`), so Weaviate's modules add weight without value. The GraphQL API is awkward to use from a Go controller compared to gRPC with typed protobuf messages. Higher memory baseline means more waste at per-KB scale.

### Pinecone

| Criterion | Assessment |
|---|---|
| Deployment model | Managed SaaS only. No self-hosted option. |
| License | Proprietary. |

**Why not**: Non-starter. agent-orc is a self-hosted Kubernetes operator. Requiring an external SaaS dependency for a core feature (vector search) breaks the deployment model, adds a network hop, introduces an external billing relationship, and makes air-gapped/on-prem deployments impossible.

### ChromaDB

| Criterion | Assessment |
|---|---|
| Language / binary | Python. Runs as a Python process with FastAPI. |
| API | REST only. No gRPC. Community Go client (not official). |
| Memory footprint | Python runtime overhead: 200–400Mi for a lightweight instance. |
| License | Apache 2.0. |

**Why not**: ChromaDB is designed for prototyping and small-scale local use. The Python runtime adds startup latency and memory overhead. No official Go client means we'd depend on a community-maintained wrapper or hand-roll REST calls. No gRPC means no efficient batched upserts. Reasonable for a Python notebook; wrong tool for a Kubernetes operator managing dozens of instances.

### pgvector (PostgreSQL extension)

| Criterion | Assessment |
|---|---|
| Deployment model | Requires a full PostgreSQL instance per KnowledgeBase, or a shared Postgres with per-KB schemas. |
| API | SQL over libpq/pgx. |
| Memory footprint | PostgreSQL: 128Mi minimum, but realistically 256Mi+ with shared buffers for reasonable query performance. |
| License | PostgreSQL License (permissive). |

**Why not**: pgvector is a good choice if you already run PostgreSQL and want to avoid a new dependency. We don't — agent-orc uses Redis for state and S3/GCS for checkpoints. Adding PostgreSQL to the dependency tree for vector search introduces a heavyweight operational dependency (backups, vacuuming, connection pooling, WAL management) that outweighs the benefit. Qdrant does one thing (vector search) and does it with zero operational overhead beyond a PVC.

---

## Decision

**Qdrant**, deployed as a per-KnowledgeBase StatefulSet managed by the operator.

### The deciding factors

1. **Resource efficiency at per-tenant scale**. We run one vector store per KnowledgeBase. Qdrant's Rust binary starts in <2s and runs at 256Mi. Milvus needs 512Mi–1Gi with embedded etcd/MinIO. Weaviate sits at 400Mi+. When a cluster has 20 KnowledgeBases, the difference between 256Mi and 512Mi per instance is 5Gi of cluster memory.

2. **Zero dependencies**. Qdrant is a single process with a single data directory. No etcd, no MinIO, no module system. The operator creates a StatefulSet with one container and one PVC — nothing else to manage, nothing else to break.

3. **Native gRPC with an official Go client**. The operator and ingestion pipeline are Go. Qdrant's gRPC API with typed protobuf messages gives us compile-time safety, efficient batched upserts (100 points per call), and no serialization overhead. Weaviate's GraphQL and Chroma's REST are workable but less natural from Go.

4. **Payload model fits our data**. Chunks carry text, doc IDs, chunk indices, and arbitrary user metadata. Qdrant stores all of this as structured payloads alongside vectors and returns them with search results — no second lookup, no separate metadata store.

5. **Deterministic IDs for idempotent ingestion**. We generate point IDs as `sha256(docID:chunkIndex)`. Qdrant accepts these as-is. Re-running ingestion upserts over existing points without duplicates. This is table stakes, but some databases (Weaviate) make it less straightforward.

6. **Apache 2.0**. No license surprises. No SSPL. No phone-home telemetry requirements.

### What we gave up

- **Milvus-scale horizontal sharding**. Qdrant supports distributed mode, but we run single-replica instances. If a single KnowledgeBase grows beyond what one node can handle (millions of vectors), we'd need to either shard the Qdrant deployment or reconsider Milvus for that specific KB. This hasn't been a concern at current scale.
- **Weaviate's built-in vectorizers**. We embed externally via the OpenAI-compatible API, so this isn't a loss — but teams that want an all-in-one solution might prefer Weaviate's module approach.
- **pgvector's SQL familiarity**. Engineers who know Postgres can query pgvector with familiar tools. Qdrant requires learning its API. The Go client abstraction in `internal/rag/qdrant.go` keeps this contained to ~250 lines.

---

## Current Integration

- **Image**: `qdrant/qdrant:v1.17.1` (configurable via Helm: `qdrant.image.repository` / `qdrant.image.tag`)
- **Protocol**: gRPC on port 6334, HTTP on port 6333 (diagnostics)
- **Go client**: `github.com/qdrant/go-client v1.17.1`
- **Wrapper**: [internal/rag/qdrant.go](internal/rag/qdrant.go) — `CreateCollection`, `Upsert`, `Query`, `ScrollAndOffset`, `GetCollectionInfo`
- **Distance metric**: Cosine
- **Batch size**: 100 points per upsert, 250 points per scroll page
- **Security**: Non-root user (1000), read-only root filesystem, no API key (cluster-internal only, no ingress)

---

## Revisiting This Decision

Reconsider if any of the following become true:

- A single KnowledgeBase regularly exceeds 1M vectors and single-node Qdrant becomes a bottleneck
- The project adopts PostgreSQL for another feature, making pgvector a zero-marginal-cost addition
- Qdrant changes its license away from Apache 2.0
- A vector database emerges with significantly better resource efficiency at per-instance scale