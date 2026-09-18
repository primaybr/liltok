# Changelog

All notable changes to Liltok will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.1-beta] - 2026-09-18

### Added
- **Multi-Model Rolling Fallback Sequence:** Implemented an automated 16-model cascade across Groq, Google Gemini, and NVIDIA NIM that sequentially fails over when primary models encounter rate limits (HTTP 429), quota exhaustion, or 5-hour session limits.
- **NVIDIA NIM Catalog Expansion:** Mapped and integrated 9 verified live free models from `build.nvidia.com` across pagination boundaries, including `meta/llama-3.2-11b-vision-instruct`, `nvidia/nemotron-3.5-lightning-30b-a3b`, `poolside/laguna-xs-2.1`, `google/diffusiongemma-26b-a4b-it`, `nvidia/nemotron-3-super-120b-a12b`, `openai/gpt-oss-20b`, `nvidia/nemotron-3-nano-omni-30b-a3b-reasoning`, `meta/muse-glimmer-30b`, and the 1M-context `nvidia/nemotron-3-ultra-550b-a55b`.
- **Context-Aware Routing:** Smart threshold routing that automatically directs requests exceeding 25,000 tokens to 1M-context models (Gemini and Nemotron-3 Ultra 550B) and bypasses 128k-limited targets for prompts above 120,000 tokens.
- **Granular Token Ledger Metrics:** Added `prompt_cost_usd` and `completion_cost_usd` columns to `request_logs` with retroactive migration backfill to compute distinct input versus output expenditures.
- **Dashboard Reset Endpoint:** Added `POST /api/v1/logs/clear` endpoint with real-time SSE broadcast (`logs_cleared`) and optional virtual key spend resets.
- **Dashboard UI/UX Overhaul:** Rebuilt the monitoring interface with 5 glassmorphism KPI cards:
  - Tokens In (Prompts) with prompt spend sub-label
  - Tokens Out (Completions) with completion spend sub-label
  - Total Token Spend with explicit input/output spend breakdown
  - Net Savings with baseline gross spend comparison
  - Cache Optimization Rate tracking local and model KV hits
- **Pure SVG Visualizations:** Embedded zero-dependency SVG statistical charts:
  - Inference Activity & Latency Sparkline with gradient area fill, peak latency, and call counters
  - Cache Tier Distribution Donut Chart with live optimization percentage
  - Provider Routing Share horizontal progress bars
- **Pricing Transparency Indicators:** Added amber `[ESTIMATED]` badges across all spend metrics, savings counters, virtual key summaries, and table headers.
- **Model Reflection Provenance:** Separated appointed upstream execution models from client requested models in SQLite schema, ledger, and UI with `from <requested-model>` attribution.
- **Starter Pack Expansion:** Merged local SQLite cache records into the embedded starter pack (`internal/db/starter_cache.json.gz`), expanding zero-cold-start pre-seeded entries to 382 canonical items (939 KB compressed).
- **Asymmetric Moderation System:** End-to-end RSA/AES-GCM encryption for moderation audit records and an embedded Private Moderator Studio (`/moderator`) with in-browser key generation.

### Changed
- Replaced local Ollama fallback with resilient, zero-cost cloud tiers (Groq, Gemini, NVIDIA NIM).
- Updated default version constant to `0.1.1-beta` across CLI, MCP server metadata, HTTP admin endpoints, and cache sync user-agent headers.
- Deferred Server-Sent Events (SSE) connection until window `load` event to eliminate browser document hang during page navigation.
- Pricing registry now credits full baseline dollar savings while assigning $0.00 cost to free-tier provider executions.

### Fixed
- **Cross-Protocol Tool Calling:** Added bi-directional conversion between Anthropic Messages API tool definitions and OpenAI/Gemini function specifications.
- **Gemini Schema Sanitization:** Stripped unsupported JSON Schema keywords (`default`, `minItems`, `maxItems`) that cause Gemini `INVALID_ARGUMENT 400` errors.
- **Gemini Array Items Repair:** Added defensive handling for array schema types with missing or boolean `items` attributes.
- **Plaintext Tool Extraction:** Added fallback parser to intercept markdown JSON tool calls emitted by models that do not support native function calling.
- **SSE Row Deduplication:** Prevented duplicate log rows during SSE reconnects using an in-memory sliding window Set.
- **Database Windows File Locking:** Resolved daemon file locking conflicts during iterative builds.

---

## [0.1.0-beta] - 2026-09-15

### Added
- Initial beta release of Liltok AI Gateway & Router.
- Multi-tier caching engine: Tier-1 Exact Match, Tier-2 KV Prefix Cache, and Tier-3 Semantic Vector Cache.
- OpenAI and Anthropic protocol translation layers.
- Virtual API key management with rate limits, spend caps, and model access policies.
- Real-time developer dashboard with live inference streaming and cache explorer.
- SQLite persistent storage with WAL mode enabled.
- Starter pack embedding for instant 0ms responses to common coding prompts.
