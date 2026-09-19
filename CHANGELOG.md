# Changelog

All notable changes to Liltok will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.2-beta] - 2026-09-19

### Added
- **Kilo AI Gateway Integration:** Integrated Kilo AI Gateway (`https://api.kilo.ai/api/gateway`) supporting completely keyless, anonymous inference on free tier models.
  - Dynamic discovery and catalog synchronization querying `GET /api/gateway/models`.
  - Filtered 381 models down to 21 verified active free reasoning and chat models (`pricing.prompt == "0"` and `pricing.completion == "0"`).
  - Excluded audio and multimodal models (`google/lyria-*`) and safety guardrails.
  - Native ingestion of context windows up to 262,144 tokens.
  - Shorthand alias translation (`kilo/free`, `kilo/auto`, and `kilo-auto` -> `kilo-auto/free`, `deepseek-r1` -> `deepseek/deepseek-v4-flash-0731:free`).
  - Zero-rate token pricing ($0.00) in the tokens pricing engine.
- **Mistral AI Free Integration:** Integrated Mistral AI (`https://api.mistral.ai/v1`) with free experimentation tier support.
  - Intelligent filtering of models that encounter HTTP 429 rate limit exceptions on free tier accounts (`mistral-small-*`, `mistral-medium-*`, `labs-*`) and non-chat models (`mistral-embed`, `mistral-ocr`).
  - Native ingestion of context windows up to 262,144 tokens across verified working models: `codestral-latest` (256k), `ministral-8b-latest` (262k), `ministral-3b-latest` (131k), `ministral-14b-latest` (262k), and `voxtral-small-latest` (32k).
  - Shorthand alias translation (`mistral/codestral` -> `codestral-latest`, `mistral/ministral-8b` -> `ministral-8b-latest`).
  - Zero-rate token pricing ($0.00) in the tokens pricing engine.
- **OpenRouter Free Tier Integration:** Dynamic synchronization of 22 active free chat and reasoning models from `https://openrouter.ai/api/v1/models`.
  - Filtered out 420+ paid models, Lyria audio models, and moderation guardrails.
  - Ingested native context lengths up to 1,048,576 tokens (`deepseek/deepseek-v4-flash-0731:free`, `openrouter/free`).
  - Automated remapping for decommissioned models (`deepseek-r1:free` -> `deepseek/deepseek-v4-flash-0731:free`, `openrouter/auto` -> `openrouter/free`).
- **NVIDIA NIM Automated Dynamic Discovery:** Dynamic catalog synchronization and model validation querying `https://integrate.api.nvidia.com/v1/models`.
  - Filtered out 50+ decommissioned 404 endpoints, embeddings, video detectors, and translation models to maintain 16 verified working chat and reasoning models.
  - Automated translation of deprecated model IDs to active reasoning models (`llama-3.1-70b-instruct` -> `nemotron-3.5-lightning`, `deepseek-r1` -> `deepseek-v4-flash-0731`).
- **Groq Dynamic Model Discovery:** Automated model synchronization and active catalog management from `https://api.groq.com/openai/v1/models`.
  - Filtered out deprecated models and audio/whisper endpoints, keeping active reasoning models (`qwen/qwen3.8-27b`, `openai/gpt-oss-120b`, `openai/gpt-oss-20b`).
- **Synthetic Cache Mining Studio:** Interactive browser-based studio in the developer dashboard for pre-warming caches with domain prompts.
  - 5 prompt categories: Coding Idioms, Compiler Errors, Security, DevOps, Git.
  - Concurrent worker pool with customizable RPM rate limits and target models selection.
  - Live log streaming and hit-rate audit table.
- **Dashboard Tagline & Efficiency Metric:**
  - Added company tagline in header: `Little Token, Big Savings.` adjacent to the application logo.
  - Added real-time efficiency percentage badge (`XX.X% EFF`) to the Net Savings KPI card.
  - Compacted baseline gross spend display across dashboard views.
  - Added Kilo and Mistral provider selections and brand styling across log filters and mining controls.

### Changed
- Updated default version to `0.1.2-beta` across CLI, MCP server metadata, HTTP admin endpoints, and cache sync user-agent headers.
- Expanded `auto-resilient` and `free-first` routing fallback sequences to include Kilo and Mistral free endpoints.
- Quarantined disabled Gemini service account key from runtime provider rotation.

### Fixed
- **OpenAI & Anthropic Parameter Translation:** Preserved tool signatures and fixed argument order in provider adapter constructors.
- **Kilo Anonymous Gateway Access:** Allowed Kilo health checks and model listings to proceed cleanly when no API key is configured.
- **Windows Binary File Locking:** Terminated active daemon background tasks prior to compilation across all build steps.

---

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
