# Semantic Search Deployment Guide

Memos supports local, private semantic search over your notes using [Ollama](https://ollama.com/)
for embeddings and [chromem-go](https://github.com/philippgille/chromem-go) as an embedded vector
store. Memo content is split into token-budgeted chunks before embedding, so long notes can be
matched by the most relevant section rather than by a single whole-note vector.

This guide covers a fresh deployment end-to-end: prerequisites, Memos configuration, usage, and
troubleshooting.

## Table of contents

- [Architecture at a glance](#architecture-at-a-glance)
- [Prerequisites](#prerequisites)
- [Configure Memos](#configure-memos)
- [Use semantic search](#use-semantic-search)
- [Tuning](#tuning)
- [Data layout and backup](#data-layout-and-backup)
- [Reset and rebuild the index](#reset-and-rebuild-the-index)
- [Verify via the HTTP API](#verify-via-the-http-api)
- [Troubleshooting](#troubleshooting)
- [Known limitations (v1)](#known-limitations-v1)

---

## Architecture at a glance

```
+-------------+     embed query      +-------------------+
|   Memos     | -------------------> | Ollama (local)    |
|  backend    | <------------------- | /v1/embeddings    |
|             |   vector             | (OpenAI-compat)   |
|  chromem-go |                      +-------------------+
|  (vector    |
|   DB + gob  |     SQL memo         +-------------------+
|   sidecar)  |     CRUD            |   SQLite/MySQL/PG |
+-------------+ ------------------> +-------------------+
```

- **Provider model**: reuses the existing OpenAI-compatible AI provider abstraction
  (`internal/ai/`). Ollama's `/v1/embeddings` endpoint speaks the OpenAI protocol, so
  you configure it as a `OPENAI`-type provider with a custom endpoint.
- **Vector store**: `chromem-go` is embedded directly in the Memos process — no extra
  service to run. Persistence is a gzip-compressed file plus a small sidecar index for
  orphan reconciliation.
- **Indexing pipeline**: hybrid. Writes (`CreateMemo`/`UpdateMemo` and Markdown zip import)
  trigger async chunk upserts; a background runner reconciles and backfills periodically.
- **Chunking**: each memo is stripped to plain text, split with Markdown / Chinese / English
  boundaries, then capped by a token budget. Search ranks chunks first and returns memo-level
  results with the best matching chunk snippet.

---

## Prerequisites

### 1. Install Ollama

Pick one of:

- **macOS / Linux / Windows installer**: <https://ollama.com/download>
- **Docker**:
  ```bash
  docker run -d --name ollama -p 11434:11434 -v ollama:/root/.ollama ollama/ollama
  ```
- **Linux install script**:
  ```bash
  curl -fsSL https://ollama.com/install.sh | sh
  ```

Confirm Ollama is serving:

```bash
curl http://localhost:11434/api/tags
# Should return JSON listing locally available models.
```

If Memos runs in Docker and Ollama runs on the host, use
`http://host.docker.internal:11434/v1` as the endpoint from inside the container.
If both run in Docker, put them on the same network and address Ollama by container name.

### 2. Pull an embedding model

Pick one of the recommended embedding models (or any other you trust):

```bash
ollama pull nomic-embed-text    # 137M, 768-dim, solid for EN + ZH
# ollama pull bge-m3            # 567M, 1024-dim, stronger multilingual
# ollama pull mxbai-embed-large # 670M, 1024-dim, strong general purpose
```

Note the exact model name — you'll need it in the Memos configuration below.

> Embedding models are different from chat models. Do **not** use models like
> `llama3`, `qwen`, or `gpt-oss` — they don't expose `/v1/embeddings` and the runner
> will fail with `404 model not found`.

### 3. Run Memos

Optional: shorten the index interval so you can see backfill activity immediately.

```bash
# Source
MEMOS_MEMOINDEX_INTERVAL=30s go run ./cmd/memos --port 5230

# Docker
docker run -d \
  -e MEMOS_MEMOINDEX_INTERVAL=30s \
  -p 5230:5230 \
  -v ~/.memos:/var/opt/memos \
  neosmemo/memos:stable
```

If you omit `MEMOS_MEMOINDEX_INTERVAL`, the default is 5 minutes.

---

## Configure Memos

### Step 1 — Add an AI provider

Open the web UI at <http://localhost:5230>, sign in as an admin, then:

**Settings → AI → AI Integrations → Add Provider**

| Field | Value |
| --- | --- |
| Title | `Local Ollama` (any friendly name) |
| Type | **OPENAI** (critical — do not pick anything else) |
| Endpoint | `http://localhost:11434/v1` |
| API Key | `ollama` (any non-empty string; Memos requires this field, Ollama ignores it) |

Click **Save**. The integration shows up in the providers table with a redacted key hint.

### Step 2 — Configure embedding

On the same **Settings → AI** page, scroll past the *Transcription* section to the
new **Semantic search** section.

| Field | Value |
| --- | --- |
| Provider | pick `Local Ollama` (the integration from Step 1) |
| Model | `nomic-embed-text` — must exactly match the name returned by `ollama list` |

Click **Save**.

### Step 3 — Confirm the index is building

Watch the Memos backend logs. Within one index interval (≤ 30s with the env var above)
you should see:

```
memoindex runner started interval=30s
memoindex pass complete upserted=N reconciled_deleted=0 valid_in_sql=N
```

`upserted=N` is the count of your existing memos that got embedded in that pass. On
first enable, `N` equals your total memo count; subsequent passes show small deltas
as you create or edit memos.

If you see `memoindex disabled: embedding not configured or vector store init failed`
after saving the config, **restart Memos** — the vector store is initialized at
startup based on the persisted AI setting.

---

## Use semantic search

The sidebar search bar gains a mode toggle in the top-right:

| Icon | Mode | Behavior |
| --- | --- | --- |
| `Aa` (Type) | Substring | Original `content.contains(...)` behavior, unchanged |
| ✨ (Sparkles) | Semantic | Embeds the query, ranks memos by cosine similarity |

Click the icon to toggle. In semantic mode:

- Placeholder changes to *"Search memos by meaning…"*
- Type a natural-language query and press **Enter**
- Results render inline below the input, each tagged with `NN% match` and the best matching
  chunk snippet

The toggle is purely client-side state — no page reload required.

### Examples

If you have notes about "deploying PostgreSQL replicas" and "running Kubernetes in
production", all of the following queries should surface them:

- `how do I scale my database`
- `容器编排` (cross-language works because the embedding model is multilingual)
- `high availability setup`

Substring search would find none of these without exact word overlap.

---

## Tuning

### Index interval

The runner wakes every `MEMOS_MEMOINDEX_INTERVAL` to backfill missing embeddings and
reconcile deleted memos against the SQL store.

| Workload | Suggested |
| --- | --- |
| Personal, < 1k memos, local Ollama | `5m` (default) |
| Team / large archive, remote OpenAI endpoint | `15m`–`1h` |
| First-time setup / debugging | `30s` |

Values below 1 minute are not recommended in production — each pass does a full SQL
scan plus full sidecar scan regardless of changes.

### Switching embedding models

Different models produce different vector dimensions, so chromem-go stores them in
separate collections. Changing the model in Settings:

1. Creates a new empty collection `memos-chunk-v1-<newmodel>`
2. Leaves the old collection and sidecar on disk (orphaned; not auto-deleted)
3. Triggers full backfill on the next runner pass (CPU/IO intensive)

Until the backfill completes, search results will be incomplete. For a clean cutover,
stop Memos, delete `vector-db/`, restart, then change the model in Settings.

---

## Data layout and backup

When semantic search is enabled, Memos creates the following directory inside its data
folder (`/var/opt/memos` in Docker, `~/.memos/` or `./` locally by default):

```
<vector-db>/
├── memos-chunk-v1-<model>.chromem-go          # chromem-go persistent file (gzip)
└── memo-index-memos-chunk-v1-<model>.gob      # sidecar: indexed memo IDs → content SHA
```

**Backup**: include the data directory in your existing Memos backup. The vector files
travel alongside the SQLite database.

**Restore**: stop Memos, restore the data directory, start Memos. The runner picks up
the persisted state automatically.

**Migration to a new host**: copy the data directory to the new host. No rebuild needed.

---

## Reset and rebuild the index

Use this when you suspect corruption, want to reclaim disk after switching models, or
need to force a clean re-embedding.

```bash
# 1. Stop Memos
# 2. Remove the vector store
rm -rf /var/opt/memos/vector-db
# 3. Start Memos
```

On startup the runner sees an empty vector store and rebuilds from SQL on the next pass.
This is safe — the SQL store is always the source of truth for memo content.

---

## Verify via the HTTP API

For programmatic verification or integration:

```bash
TOKEN=...   # generate a personal access token in Settings → Access tokens

curl -X POST http://localhost:5230/api/v1/memos:search \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"query":"how to deploy k8s","topK":10}'
```

Response:

```json
{
  "results": [
    {"memo": "memos/abc123", "similarity": 0.87, "snippet": "...", "chunkIndex": 2},
    {"memo": "memos/def456", "similarity": 0.74, "snippet": "...", "chunkIndex": 0}
  ]
}
```

Error responses:

| Code | Cause |
| --- | --- |
| `UNAUTHENTICATED` | Missing or invalid token |
| `INVALID_ARGUMENT` | Empty query, query > 1024 chars, or `filter` contains a `content.*` predicate |
| `FAILED_PRECONDITION` | Embedding not configured, or vector store failed to initialize at startup |
| `INTERNAL` | Ollama unreachable, embedding call failed, or SQL store error |

---

## Troubleshooting

| Symptom | Check |
| --- | --- |
| No **Semantic search** section on the AI settings page | Add at least one `OPENAI`-type provider first — the section appears below the *AI Integrations* table |
| UI shows *"Semantic search is not configured"* | Embedding config not saved, or server started before config existed — restart Memos |
| Logs show `memoindex upsert failed err=...connection refused` | Ollama not running, endpoint URL wrong, or port blocked by firewall |
| Logs show `404 model not found` | Model field doesn't match `ollama list` output exactly. Use `nomic-embed-text`, not `nomic-embed-text:latest` |
| `failed to open chromem-go persistent DB: permission denied` | Memos process can't write to `profile.Data/vector-db`. Fix the data directory permissions |
| Switched model, now no results | New model → new empty collection → backfill in progress. Wait for the next runner pass or delete `vector-db/` to force a rebuild |
| CPU / memory spike | Backfill is CPU-bound on local Ollama. Increase `MEMOS_MEMOINDEX_INTERVAL`, use a smaller embedding model, or run Ollama on GPU |
| Search is slow (> 1s per query) | Large vector store + CPU. Consider `bge-m3` only if you need multilingual depth; `nomic-embed-text` is faster |
| Embeddings work but CreateMemo doesn't trigger indexing | Confirm the embedding config is still saved; the write hook is a no-op when `VectorStore` is nil |

Enable debug logs for the indexing pipeline:

```bash
MEMOS_LOG_LEVEL=debug MEMOS_MEMOINDEX_INTERVAL=30s go run ./cmd/memos
```

---

## Known limitations (v1)

- **Per-user scoping**: search returns only memos created by the calling user
  (filtered at the vector layer via `creator_id` metadata). Other users' `PUBLIC`
  memos are not surfaced in semantic results in v1.
- **No `content.*` filter predicates**: CEL filters passed to `SearchMemos` may
  scope by `creator`, `visibility`, `tag`, etc., but `content.contains(...)` is
  rejected — semantic similarity subsumes substring matching.
- **Ollama downtime**: write-path indexing is async and best-effort. If Ollama is
  unreachable when a memo is created, the memo is added to SQL normally and gets
  embedded on the next runner pass. Searches, however, are synchronous and will
  surface the error to the user.
- **No hot reload of the index interval**: changing `MEMOS_MEMOINDEX_INTERVAL`
  requires a process restart.
- **Switching models or chunk strategy leaves orphaned collections**: previous collections stay
  on disk; reclaim space by deleting `vector-db/` and restarting.
- **No built-in monitoring**: no metrics endpoint for index size or query latency
  yet. Watch logs and the on-disk `vector-db/` size.
