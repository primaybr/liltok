# liltok: The Open-Source Local AI Gateway & Router

<p align="center">
  <strong>Little Token, Big Savings.</strong><br>
  An ultra-fast, locally-hosted AI Gateway and Router engineered to slash token consumption and eliminate SaaS gateway fees through intelligent multi-tier caching, prompt compression, and resilient multi-provider routing.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-1.22%2B-blue.svg" alt="Go Version">
  <img src="https://img.shields.io/badge/binary-zero--cgo-success.svg" alt="Zero CGO">
  <img src="https://img.shields.io/badge/cache-multi--tier-emerald.svg" alt="Multi-Tier Cache">
  <img src="https://img.shields.io/badge/license-MIT-purple.svg" alt="License">
</p>

---

## Key Capabilities

- **Sub-Millisecond Zero-Copy Proxying**: Written in pure Go (`CGO_ENABLED=0`) with zero runtime daemon dependencies and zero intermediate buffer allocations.
- **Dual Ingress Architecture**: Native support for standard OpenAI SDKs (`/v1/chat/completions`) and native Anthropic Messages API (`/v1/messages`), enabling instant zero-config drop-in for **Claude Code in VS Code**, Cursor, Aider, and custom agents.
- **Multi-Tier Caching Pipeline**:
  - **Tier-0 Pre-Flight Compactor**: RTK-inspired token compression stripping git index metadata, compacting context padding ($> 3$ lines), collapsing ASCII directory trees, and stripping comment dividers.
  - **Tier-1 Exact Match Cache**: SHA-256 canonical parameter normalizer with in-memory L1 LRU ($TTFT < 1\mu\text{s}$) backed by persistent SQLite WAL.
  - **Tier-2 Prefix & Prompt Cache**: Automatic injection of Anthropic ephemeral cache control blocks (`"cache_control": {"type": "ephemeral"}`) on prompts $\ge 1,024$ tokens for up to 90% prompt cost savings.
  - **Tier-3 Semantic Similarity Cache**: Pure-Go local cosine similarity matching ($\ge 0.95$) with fast vector indexing and safety guardrails.
- **Resilient Cross-Protocol Routing**: Translates Claude Code requests to run on free high-speed models (NVIDIA NIM Llama 3.3 70B, Groq, local Ollama) for **$0.00 spend**, guarded by automated 3-state Circuit Breakers.
- **Virtual Keys & Hard Monthly Spend Quotas**: Generate virtual client tokens (`lt-live-xxxx`), enforce Token Bucket rate limits (RPM/TPM), and set hard monthly dollar budgets. When spend is exceeded, paid upstreams are blocked with HTTP 429 (`insufficient_quota`) while free cache hits continue uninterrupted.
- **Embedded Dark-Mode Dashboard**: Zero-CDN Single Page Application embedded statically into the Go binary (`/dashboard`) with real-time SSE request streaming, cache inspection, and key management.
- **OpenMetrics / Prometheus Exporter**: Native `/metrics` endpoint exporting uptime, cache entries, request volume, token usage, and circuit breaker states.

---

## Architecture Overview

```
                      +------------------------------------------+
                      |         Developer AI Coding Agents       |
                      |   Claude Code | Cursor | Aider | SDKs    |
                      +------------------------------------------+
                                           |
                                   HTTP Dual Ingress
                                           v
+-----------------------------------------------------------------------------------+
| liltok Gateway Server (:8080)                                                     |
|                                                                                   |
|  [ Auth & Quota Enforcer ]  --> Check Virtual Key Token Bucket & Monthly Budget   |
|            |                                                                      |
|            v                                                                      |
|  [ Tier-0 Token Pruner ]    --> Compact Unified Diffs & ASCII Directory Trees     |
|            |                                                                      |
|            v                                                                      |
|  [ Tier-1 Exact Cache ]     --> Canonical Hashing -> L1 Memory LRU / SQLite WAL   |
|            | (HIT: $0.00, < 1ms)                                                  |
|            v (MISS)                                                               |
|  [ Tier-3 Semantic Cache ]  --> Fast Local Embedder -> Cosine Sim >= 0.95         |
|            | (HIT: $0.00, < 2ms)                                                  |
|            v (MISS)                                                               |
|  [ Resilient Router ]       --> 3-State Breaker -> Fallback Routing Strategy      |
|            |                                                                      |
|            +-----------------------+-----------------------+                      |
|            |                       |                       |                      |
|            v                       v                       v                      |
|   OpenAI / Anthropic           NVIDIA NIM / Groq       Local Ollama               |
|   (Tier-2 Prefix Cache)        (100% Free Coding)      (Offline $0.00)            |
+-----------------------------------------------------------------------------------+
```

---

## Quickstart

### 1. Download or Build

Download pre-compiled standalone static binaries from `bin/` or build from source:

```bash
# Build natively (requires Go 1.22+)
go build -ldflags "-s -w" -o bin/liltok ./cmd/liltok
```

Cross-compiled static binaries with embedded dashboard assets:
- **Linux (x86_64)**: `bin/liltok-linux-amd64`
- **Linux (ARM64)**: `bin/liltok-linux-arm64`
- **macOS (Apple Silicon)**: `bin/liltok-darwin-arm64`
- **macOS (Intel)**: `bin/liltok-darwin-amd64`
- **Windows (x86_64)**: `bin/liltok-windows-amd64.exe`

### 2. Initialize Configuration

```bash
./liltok init
```
This initializes `~/.liltok/liltok.yaml` and sets up the SQLite database at `~/.liltok/liltok.db`.

Edit `~/.liltok/liltok.yaml` with your upstream API keys:
```yaml
server:
  host: "127.0.0.1"
  port: 8080

providers:
  openai:
    api_key: "sk-..."
  anthropic:
    api_key: "sk-ant-..."
  nvidianim:
    api_key: "nvapi-..."
  groq:
    api_key: "gsk_..."
  ollama:
    base_url: "http://localhost:11434"

routes:
  default_strategy: "auto-resilient" # auto-resilient | free-first | premium-only
```

### 3. Start the Gateway

```bash
./liltok start
```

---

## Developer Tooling Setup

### Claude Code (VS Code & CLI)

To route **Claude Code** through Liltok:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=your-anthropic-key-or-liltok-virtual-key
claude
```

> **Zero-Cost Coding Mode**: Set `routes.default_strategy: "free-first"` in `liltok.yaml`. Liltok's cross-protocol reverse translator intercepts Claude Code's Messages API requests and translates them dynamically to NVIDIA NIM (Llama 3.3 70B) or local Ollama for **$0.00**.

### Cursor & Windsurf

In Cursor settings under **OpenAI API Key**:
1. Check **Override OpenAI Base URL**.
2. Set URL to: `http://localhost:8080/v1`.
3. Enter your upstream OpenAI key or a Liltok Virtual Key (`lt-live-xxxx`).

### Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="lt-live-xxxx" # or your upstream key
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Explain SQLite WAL mode."}]
)
print(response.choices[0].message.content)
```

---

## CLI Reference

### Virtual Key Management (`liltok keys`)

Generate and manage scoped API keys with hard spend caps and token bucket rate limits:

```bash
# Create a virtual key with a $25.00 monthly cap and 60 RPM limit
liltok keys create --name "cursor-dev" --budget 25.00 --rpm 60 --tpm 100000

# List all virtual keys and track real-time spend
liltok keys list

# Revoke a virtual key
liltok keys revoke key_d741473d
```

### Cache Management (`liltok cache`)

```bash
# View cache entry count and hit statistics
liltok cache stats

# List recent cached prompt entries with preview
liltok cache list

# Purge cache entries
liltok cache purge --hash <sha256-hash>
liltok cache purge --all
```

---

## Observability & Developer Dashboard

### Web Dashboard (`http://localhost:8080/dashboard`)
Open `http://localhost:8080/dashboard` in any browser:
- **Live Stream**: Real-time request visualizer with green-flash animations on cache hits.
- **Cache Explorer**: Browse cached prompts, view hit counts, inspect TTLs, and evict individual keys.
- **Key Manager**: UI to create virtual keys, set budgets, and monitor monthly spend.
- **Breaker Health Grid**: Live status cards for upstream providers (`CLOSED`, `HALF-OPEN`, `OPEN`).

### Prometheus / OpenMetrics (`GET /metrics`)
Scrape metrics directly for Prometheus or Grafana dashboards:
- `liltok_uptime_seconds`
- `liltok_cache_entries_total`
- `liltok_requests_total{model, cache_status, cache_tier}`
- `liltok_tokens_total{model, type}`
- `liltok_savings_usd_total{model}`
- `liltok_cost_usd_total{model}`
- `liltok_circuit_breaker_state{provider}`

---

## Performance Benchmarks

Micro-benchmark results executed on Go 1.22+ (`Intel i7-8750H @ 2.20GHz`):

| Component | Benchmark Operation | Latency / Throughput | Memory Allocations |
|---|---|---|---|
| **L1 LRU Cache** | In-memory read hit | **624 ns/op** ($> 1.6\text{M ops/sec}$) | $21\text{ B/op}$ (1 alloc) |
| **Normalizer** | Full JSON normalization & SHA-256 | **20.4 \mu\text{s/op}** ($> 48\text{k ops/sec}$) | $3.4\text{ KB/op}$ (67 allocs) |
| **Token Pruner** | Git unified diff context compaction | **9.1 \mu\text{s/op}** ($> 100\text{k ops/sec}$) | $4.7\text{ KB/op}$ (18 allocs) |
| **Semantic Embedder** | Fast local stop-word vector embedding | **6.2 \mu\text{s/op}** ($> 160\text{k ops/sec}$) | $1.7\text{ KB/op}$ (6 allocs) |
| **Cosine Similarity**| 256-dimensional vector dot product | **591 ns/op** ($> 1.6\text{M ops/sec}$) | **0 B/op** (0 allocs) |
| **Pricing Engine** | Dynamic cost & savings calculation | **213 ns/op** ($> 4.6\text{M ops/sec}$) | **0 B/op** (0 allocs) |

---

## Architecture Documentation Suite

For deep architectural specifications, refer to the [`docs/`](docs/) directory:
- [`docs/ROADMAP.md`](docs/ROADMAP.md): Complete milestone specification (M0 to M6).
- [`docs/SRS.md`](docs/SRS.md): Software Requirements, APIs, SQLite schemas.
- [`docs/BRD.md`](docs/BRD.md): Business requirements and token economics.
- [`docs/PRD.md`](docs/PRD.md): Product requirements and Gherkin user stories.
- [`docs/FRD.md`](docs/FRD.md): Multi-tier caching and reverse proxy specifications.
- [`docs/DESIGN_PRINCIPLE.md`](docs/DESIGN_PRINCIPLE.md): Architectural latency budget and UI design.

---

## License

MIT License. Engineered for developers, AI agents, and cost-conscious engineering teams.
