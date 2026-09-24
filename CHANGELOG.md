# Changelog

All notable changes to Liltok will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Translation Replay Harness:** `internal/proxy/testdata/replay/*.json` fixtures pair a Claude Code `/v1/messages` request with scripted upstream replies per fallback attempt. Each runs through the real handler (pruner, router validators, translator, SSE replay) in JSON and streaming mode and checks the content blocks, stop reason, attempt count, and upstream request text. The first 13 fixtures cover the empty-turn, thinking-split, tool-name, text tool-call, repetition-loop, tool-output, plan-mode, and chain-exhaustion fixes from 0.1.9 to 0.2.1. `Router.SetRoute` registers a named route.
- **Replay Fixture Capture:** set `routes.capture_dir` (or `LILTOK_CAPTURE_DIR`) to record a fixture for every routed `/v1/messages` request answered by a translated provider. Each file holds the original request, the raw upstream reply of every fallback attempt, and expectations that snapshot the response sent. Correct the expectations and move the file into `internal/proxy/testdata/replay/` to turn a live failure into a regression test. Captures contain the full conversation, so the option is off by default and files are written owner-only. `router.WithAttemptObserver` exposes the raw attempts.

### Changed
- **Miner Default Target Models:** mined entries are now stored under `claude-opus-5-5` and `claude-haiku-4-5-20251001` instead of `claude-opus-5` and `claude-haiku-4-5`. Cache keys hash the exact model string, and Claude Code sends the new IDs, so entries under the old IDs produced almost no hits. The CLI, dashboard, admin API, and corpus audit share one default list (`miner.DefaultTargetModels`). The old IDs remain selectable in the dashboard. The `liltok_ask` MCP tool now defaults to `claude-opus-5-5`.
- **Premium-Only Route:** targets `claude-opus-5-5` instead of `claude-opus-5`.

## [0.2.1-beta] - 2026-09-24

### Added
- **Per-Attempt Upstream Timeout:** each non-premium attempt in a fallback chain is limited to `routes.attempt_timeout_seconds` (default 45, `LILTOK_ATTEMPT_TIMEOUT_SECONDS`, 0 disables). A provider that accepts a request and never answers now hands over to the next target instead of holding the request for the full 120-second HTTP timeout. A timed-out attempt counts as a provider failure. Premium providers (Anthropic, OpenAI) are not limited, so long direct completions are not cut off.

### Fixed
- **Failover After Client Disconnect:** when the client cancels a request, the fallback chain stops immediately. Previously every remaining target was tried and failed instantly with `context canceled`.
- **Dashboard Provider Keys Lost on Restart:** saving a provider's key from the dashboard only rewrote `api_key` lines that already existed in the config file, so a provider missing from the file (such as Cline in configs created before it was added) kept the key in memory only, and it was lost on restart. The save now edits the parsed YAML, adding missing provider blocks and `api_key` lines while keeping comments and other settings. It creates the config file if it doesn't exist, and a failed write is now reported by the dashboard instead of showing success. The file is written with owner-only permissions.
- **Circuit Breakers Charged for Client Cancels:** `context canceled` errors no longer count as provider failures, so a disconnecting client no longer degrades the breakers of every provider left in the chain.

## [0.2.0-beta] - 2026-09-24

### Added
- **Tool-Name Validation:** returned tool calls are checked against the request's declared tools. Case or underscore mismatches are repaired (`glob` -> `Glob`); undeclared names (`Global`) trigger rolling failover.

### Changed
- **Claude Code Plan Mode:** an `ExitPlanMode` issued before the plan file is written is replaced with a `Write` of the plan to the path named in the plan-mode reminder, and the plan is shown as chat text. Claude Code reads the plan from that file. `ExitPlanMode` with no plan text anywhere now triggers failover instead of reaching the user as an empty plan.
- **Tool Output Preserved Verbatim:** the pre-flight pruner no longer rewrites `tool_result` or `role: "tool"` content. Stripped trailing whitespace and collapsed lines broke exact-match `Edit` calls. Oversized historical tool output is still reduced by the session compactor.

### Removed
- **Mistral Provider:** removed from routing, configuration, dashboard, miner, and pricing due to 200+ second upstream latency. A `providers.mistral` block in existing configs is ignored.

### Security
- **Cline API Key No Longer Built In:** the Cline key hardcoded in the default configuration since 0.1.3-beta is removed. Set `providers.cline.api_key` or `CLINE_API_KEY` to keep using Cline. The old key is in git history and in earlier release binaries and must be treated as exposed.

### Fixed
- **Empty Turns:** completions whose only output is a `<think>` block are now rejected as empty and fail over, instead of reaching Claude Code as a blank turn.
- **Repetition Loop Breaker:** Claude Code `<system-reminder>` messages no longer count as human input, so loop detection stays active during autonomous runs. Real user messages still exempt retries.

---

## [0.1.9-beta] - 2026-09-21

### Added
- **Claude Code Plan Mode Presentation Interceptor:**
  - Standardized `ExitPlanMode` tool name across casing and snake_case variants (`exit_plan_mode`, `exitPlanMode`, `EXIT_PLAN_MODE`).
  - Extracted implementation plans from `ExitPlanMode` tool arguments (`plan`, `plan_content`, `implementation_plan`, `description`, `summary`, `steps`, raw strings) and `<think>` reasoning traces.
  - Guaranteed emission of a human-facing `type: "text"` block preceding the `type: "tool_use"` block, ensuring complete plan markdown is rendered in Claude Code's terminal chat before prompting for approval.
  - Added structured formatting for `"steps"` arrays into numbered markdown lists.
  - Synthesized `ExitPlanMode` tool calls when open-weights models output a plan and announce exiting plan mode without formal tool markup.
  - Preserved illustrative markdown bash code blocks within implementation plans, preventing them from being converted into executable `Bash` tool calls.
- **Compact Single-Line Navigation UI & Enhanced Cache Explorer:**
  - Redesigned navigation bar to a compact, single-line layout (`.nav-compact-row`) with inline status pills, live metrics, and quick actions.
  - Upgraded Cache Explorer with detailed cache inspector, advanced multi-field filtering (search query, tier, hit count, latency saved, date), entry inspector modal, copy response button, and detailed metadata breakdown.

### Fixed
- **Claude Code Mid-Task Script Exposure & Pause Loop Elimination:**
  - Enforced `tool_use` stop reason priority over `max_tokens` when tool calls or `finish_reason: "tool_calls"` exist, eliminating Claude Code "continue" pause loops.
  - Implemented `RepairJSON` state machine for unclosed JSON strings, bracket/brace nesting stacks, and trailing commas.
  - Implemented `repairBashCommand` to automatically terminate unclosed heredocs (`cat << 'EOF' ... \nEOF\n`).
  - Added recovery for truncated/unclosed `<tool_call>` and DSML `<|DSML|invoke>` tags.
  - Extracted markdown JSON and bash execution code blocks into `Bash` tool calls.
  - Isolated thinking and scratchpad blocks into Anthropic `type: "thinking"` content blocks with streaming support in `replayAnthropicSSE`.
  - Clamped `max_tokens` safely for Groq / NVIDIA NIM in OpenAI adapter and ensured 4096 min tokens for agentic calls.

---

## [0.1.8-beta] - 2026-09-21

### Added
- **Massive Embedded Starter Pack Expansion (12,613 Canonical Entries / 19.7 MB):**
  - Merged 3,781 sanitized local database entries into the embedded starter pack (`internal/db/starter_cache.json.gz`), scaling pre-seeded entries to 12,613 total canonical responses (19,719.2 KB compressed gzip archive / ~19.26 MB).
  - Bundles zero-cold-start 0ms responses across Claude 4/4.5 (Sonnet 4.5, Haiku 4.5), Claude 5 (Sonnet 5, Opus 5), GPT-4o, DeepSeek, and open-weights models out of the box.
- **Cline & NVIDIA NIM Free-Tier Model Modernization:**
  - Decommissioned failing `deepseek-v4` target on Cline; defaulted to `nvidia/nemotron-3.5-lightning:free` (1,000,000 token context window) and `google/gemma-4-31b-it:free`.
  - Added automatic remapping for deprecated models (`deepseek-v4`, `deepseek-r1`, `meta-llama-3.3-70b-instruct`) in `internal/router/router.go`.
  - Resolved NVIDIA NIM EOL 410 error on `meta/llama-3.3-70b-instruct` by switching default miner generator to `deepseek-ai/deepseek-v4-flash-0731` and remapping to `nvidia/nemotron-3.5-lightning-30b-a3b`.

---

## [0.1.7-beta] - 2026-09-20

### Added
- **Option B Semantic Safe Session Compactor:**
  - Dedicated code inspection protection in `internal/cache/prune/session_compactor.go` safeguarding file reads (`View`, `read_file`, `cat`) and source code snippets from truncation.
  - Added heuristic protection for git diff blocks (`diff --git`) and line-numbered listings even when tool names are omitted.
  - Tamed high-volume historical terminal scrollback (`Bash`, `exec`, `terminal`) older than 10 turns exceeding 4,000 bytes (~1,000 tokens) while preserving 1,500 bytes head and tail.
  - Automatic prompt caching bypass for direct Anthropic requests, preserving byte-for-byte KV prefix discounts.
- **Modular Curated Prompt Corpus Architecture:**
  - Migrated curated prompts from hardcoded Go structs to partitioned JSON files in `internal/miner/corpus/{language}/{year-month}/{category}.json`.
  - Enforced strict file budget (< 250 lines per JSON file) for lightweight token consumption and agent inspectability.
  - Traversal and loading via `//go:embed corpus` with hash-based deduplication and `sync.Once` memoization.
- **Corpus Expansion to 880+ Canonical Prompts:**
  - Expanded pre-warming corpus to 881 curated prompts across 8 development domains: PHP, Kubernetes, Docker, Flutter, Python, Go, C, and UI/UX.
- **Cache Mining Studio Client-Side Pagination:**
  - Interactive table pagination in `web/index.html` supporting 25, 50, 100, 250, and All records per page, eliminating DOM bloat on large corpora.
- **Mining Studio Responsiveness & Observability:**
  - Added client-side auto-polling fallback (2.5s interval) to prevent SSE connection stalls from Chrome 6-connection HTTP/1.1 limits.
  - Real-time `MINING [id]: ...` console progress logging emitted immediately upon prompt synthesis start.
- **Claude Model Standardization & Pricing Registry:**
  - Standardized target models to official Generation 4/4.5 (`claude-sonnet-4-5`, `claude-haiku-4-5`, `claude-opus-4`, `claude-sonnet-4`) and Generation 5 (`claude-sonnet-5`, `claude-opus-5`, `claude-fable-5-1`).
  - Retired deprecated Claude 3.x models from default UI and CLI options.
  - Updated `internal/tokens/pricing.go` with official Anthropic pricing rates.
  - Added quick-selection controls (All, Recommended, Clear) in Cache Mining Studio.

---

## [0.1.6-beta] - 2026-09-20

### Added
- **Session Compactor Telemetry & Accounting Pipeline:**
  - Persisted `pruned_bytes` and `pruned_tokens` columns in SQLite `request_logs` schema with automatic schema migrations in `internal/db/db.go` and `001_init_schema.sql`.
  - Integrated compaction delta tracking into the core accounting pipeline (`internal/ledger/ledger.go`, `internal/proxy/proxy.go`), capturing exact byte and token savings per upstream dispatch.
  - Added Prometheus counters `liltok_compactor_pruned_bytes_total` and `liltok_compactor_pruned_tokens_total` in `internal/metrics/prometheus.go`.
  - Exposed compaction analytics in `GET /api/v1/overview` and `GET /api/v1/logs` admin endpoints.
- **Live Compactor Visualizer Dashboard:**
  - Added real-time Compactor KPI card in `web/index.html` showing aggregate context pruned and total token savings.
  - Injected visual compaction badges and pills (`✂ -Xk tok`) in the live request stream and inspection modals with percentage context reduction metrics.
- **Starter Pack Expansion:**
  - Merged and sanitized active local SQLite cache entries into `internal/db/starter_cache.json.gz`, expanding pre-warmed canonical responses to over 2.02 MB of seed patterns.

---

## [0.1.5-beta] - 2026-09-19

### Added
- **In-Flight Session Compactor for Multi-Turn Agent Conversations:**
  - Dedicated compaction engine in `internal/cache/prune/session_compactor.go` solving massive context bloat in autonomous agent sessions (such as Claude Code ballooning past 500k tokens).
  - Preserves the 5 most recent user and tool turns completely uncompressed to maintain 100% active reasoning fidelity and immediate tool feedback.
  - Historical tool results older than recent turns are compacted via Head (250 bytes) and Tail (250 bytes) preservation, retaining command headers and trailing exit codes while discarding repetitive middle scrollback (yielding an 87.9% reduction in tool result tokens).
  - Dual protocol support for both Anthropic `tool_result` (strings and content block arrays) and OpenAI `role: "tool"` payloads.
  - Configurable via `cache.session_compactor_enabled`, `cache.recent_turns_to_keep`, `cache.compactor_head_bytes`, `cache.compactor_tail_bytes`, and corresponding environment variables.
- **Autonomous Agent Repetition Loop Breaker:**
  - Real-time repetition trap detector in `internal/router/router.go` preventing agents from getting locked in infinite failure loops (such as repeated failing bash commands with identical preamble text).
  - Normalizes and compares tool calls and argument JSON structures across turns.
  - Detects identical repeat tool calls following errors, identical 3rd repeat cycles, and autonomous text-only stalls.
  - Trips the circuit breaker immediately on repetition detection, triggering automatic rolling failover to alternate candidate models (such as Nemotron, Inkling, Gemini, or Anthropic).
  - Preserves human-in-the-loop interactions with zero false positives.

### Fixed
- **Dashboard Logo Badge Dynamic Sync:**
  - Dynamically populates and refreshes the version badge in the admin dashboard header from the `/api/v1/overview` API response, preventing stale version display.

---

## [0.1.4-beta] - 2026-09-19

### Added
- **High-Context Free Model Routing:**
  - Dynamic 4-tier context-aware routing in `DispatchChat` querying `GetModelContextWindow`.
  - Prioritizes free models supporting >=256k and 1M context windows across OpenRouter, Kilo, Mistral, and Cline.
  - Added Gemini Free Tier 250k TPM guard: prompts exceeding 250k tokens automatically bypass Gemini to prevent 429 quota exhaustion.
- **DeepSeek DSML Tool Calling & Markup Healing:**
  - Native parser in `translator.go` for DeepSeek DSML (DeepSeek Markup Language) XML blocks (`<[|｜]DSML[|｜]invoke ...>`, `<[|｜]DSML[|｜]parameter ...>`).
  - Supports both ASCII pipe (`|`) and fullwidth pipe (`｜`, `U+FF5C`), self-closing tags, attribute extraction (`string="true"`, `string="false"`), and HTML entity unescaping.
  - Added support for `<tool_call>` JSON blocks (Qwen/Hermes/Llama format).
  - Exported `StripDSMLTags` to sanitize orphan or leaked DSML tags from output text.

### Fixed
- **Claude Code Cross-Protocol Tool Calling:**
  - `ParseUnifiedRequest` in `provider.go` unpacks Anthropic `tool_result` blocks into dedicated `Role: "tool"` messages with matching `ToolCallID`, while preserving assistant `ToolCalls` and reasoning blocks.
  - OpenAI adapter `buildPayload` properly formats assistant `tool_calls: [...]` (with `content: nil` when empty) and `tool` role messages.
  - Unmarshals `reasoning_content`, `reasoning`, `thought`, and `refusal` as fallback completion content.
- **Corrupted Completion & Silent Empty Failover:**
  - In `DispatchChat`, responses with empty text and no tool calls, or responses containing only orphan/corrupted DSML tags without valid tool calls or content, are flagged as upstream failures, triggering immediate rolling failover to subsequent candidate models.
- **Mid-Conversation System Message Normalization:**
  - In OpenAI adapter `buildPayload`, mid-conversation and trailing system messages (such as Claude Code `<system-reminder>` environment updates injected after tool results) are normalized to `role: "user"` with a `[System Reminder]` prefix to satisfy OpenAI and DeepSeek chat template constraints.

---

## [0.1.3-beta] - 2026-09-19

### Added
- **Cline Free Provider Integration:** Integrated Cline API (`https://api.cline.bot/api/v1`) as an active free provider supporting free reasoning and chat models.
  - Automated dynamic model discovery querying `GET /api/v1/models`, synchronizing 22 active free models on startup and interval refreshes.
  - Excluded safety guardrails, embeddings, moderation endpoints, and Lyria audio models.
  - Shorthand alias translation and automatic remapping for deprecated models (`cline/deepseek-r1:free` -> `deepseek/deepseek-v4-flash-0731:free`, `meta-llama/*` -> `qwen/qwen3.8-27b:free`, `google/gemma-2/*` -> `google/gemma-4-31b-it:free`).
  - Added Cline targets (`deepseek/deepseek-v4-flash-0731:free`, `qwen/qwen3.8-27b:free`) to `auto-resilient` and `free-first` routing fallback sequences.
  - Zero-rate token pricing ($0.00 spend) in the token pricing engine.
  - Added Cline provider selection, color styling (#0284c7), and live mining support in Cache Mining Studio.
- **Dynamic Provider Routing Share in Dashboard:**
  - Automated dynamic rendering and color assignment for Provider Routing Share cards, eliminating manual template modifications when adding or removing providers.

### Fixed
- **Cline Response Unwrapping & Error Handling:**
  - Handled Cline's proprietary response wrapper (`{"data": {...}, "success": true}`) by automatically normalizing `RawResponse` into standard OpenAI completion JSON for downstream compatibility with OpenAI SDKs, Claude Code, Cursor, and MCP tools.
  - Handled Cline HTTP 200 error payloads (`{"error": "...", "success": false}`) and empty choice payloads in `SendChat` and `StreamChat` to trigger circuit breakers and route fallbacks cleanly.

---

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
