# Changelog

All notable changes to Liltok will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.4-beta] - 2026-09-26

### Added
- **Live Streaming for Routed Replies (M7.2, part 2; opt-in):** with `routes.live_streaming: true` (`LILTOK_LIVE_STREAMING`), a streaming Anthropic-format request answered by a routed model streams as the model writes, instead of arriving whole at the end. Each attempt reads the provider's stream and holds the reply back until it has clearly passed the checks that would fail it over: more than 400 characters of prose (so it cannot be a stall), no tool-call or reasoning markup yet (DSML, `<tool_call>`, fenced blocks, `call:`, `<think>`), and not a repeat of the previous assistant turn. From that point the text streams live; text from a later markup marker is held until the reply ends. The usual checks still run on the complete reply, and the tool calls and stop reason are produced by the same translator as the non-live path. Replies that never commit (tool calls only, short answers, possible markup) are replayed exactly as before, and plan mode always uses the whole-reply path. The trade-off: a reply that fails a check after it started streaming ends with an SSE `error` event instead of failing over. Live attempts are bounded by the attempt timeout per stream event, not in total, so a long reply that keeps producing tokens is not cut off. Verified against live providers (first text about 3 s after the request, then continuous deltas) and with headless Claude Code.
- **Daily Budgets per Key (M9.3):** virtual API keys can have a daily spend cap (`daily_budget` in `POST /api/v1/keys`, `liltok keys create --daily-budget`, and the dashboard keys form), counted per UTC day. A key that reaches its daily cap is treated like one over its monthly cap: cache hits are still served and upstream requests are blocked until 00:00 UTC. Key listings show today's spend and the daily budget (migration 008).
- **Route Editing from the CLI:** `liltok route set <id> --target provider/model [--target ...] [--strategy ...] [--description ...]` creates or replaces a route on the running gateway, and `liltok route reset <id>` restores a built-in route or deletes a custom one. Targets split at the first slash, so model names such as `deepseek/deepseek-v4-flash-0731:free` work unchanged. `liltok route status` shows each route's strategy and marks custom and edited routes. Route edits need the gateway to be running and report its validation errors directly.
- **Streaming Keep-Alive for Routed Replies (M7.2, part 1):** a streaming request answered by a free model is still validated as a whole reply (so empty turns, loops, stalls and text tool calls can fail over) before it is replayed, but the client no longer waits in silence. If the answer takes longer than 2 s, liltok sends the SSE headers and a `: liltok routing` comment at once, then a comment every 10 s until the reply is ready; SSE parsers ignore comment lines, so the stream content is unchanged. In that case `X-Liltok-Provider` and `X-Liltok-Cache-Tier` arrive as HTTP trailers, and a failure after the headers (for example an exhausted `free-first` chain) arrives as a standard SSE `error` event instead of a JSON body. Replies faster than 2 s stream exactly as before. Verified with headless Claude Code through a slow route.
- **Request Latency Histogram (M9.1):** `/metrics` now exports `liltok_request_duration_seconds` (buckets from 5 ms to 120 s, with `_sum` and `_count`) by `model` and `cache_status`, and `liltok_requests_total` gains a `status` label with the HTTP status, so Prometheus can chart P99 latency and error rates. Both are computed from `request_logs`, like the existing metrics.
- **Route Strategies (M8.1):** a route's `strategy` now decides the order its targets are tried in; every strategy still fails over through the whole list. `fallback` and `free_first` keep the listed order (unchanged behaviour), `round_robin` starts each request one target further along the list, and `least_cost` tries the cheapest targets first using the pricing table (free-tier providers cost $0; equal costs keep the listed order). Strategies apply to named routes (selected by `X-Liltok-Route` or a route name as the model); models resolved by name keep their fixed order.
- **Route Editing API (M8.2):** `PUT /api/v1/routes` creates or replaces a route (`id`, `description`, `strategy`, and `targets` as `{provider, model}` pairs); edits are validated (known strategy, registered providers, 1-100 targets) and stored in the `provider_routes` and `route_targets` tables, so they survive restarts. Saving a built-in route ID (`auto-resilient`, `free-first`, `premium-only`) overrides it; `DELETE /api/v1/routes/{id}` restores a built-in route or deletes a custom one. Saved routes that name a provider that no longer exists are skipped at startup with a warning. The dashboard shows each route's strategy and marks custom and edited routes.

### Changed
- **Test Coverage 90.9%:** router, proxy and admin tests raise total coverage from 83% to 90.9% (router 95%, proxy 96%, admin 92%), past the 85% v1.0 target, and CI's coverage floor rises from 80% to 85%.

- **Test Coverage Raised to 83%:** new tests cover the CLI commands and MCP server (0% to 93%), config, middleware, the provider adapters, the miner, cache sync and the database layer (each now 92-100%), and CI's coverage floor rises from 63% to 80%. `main` now only runs `newRootCommand()`, so the command tree can be built in tests.

### Fixed
- **Monthly Key Budgets Never Reset:** a key's monthly spend only ever grew, so a key that reached its monthly budget stayed blocked from upstream requests permanently (only a manual spend reset cleared it). Spend is now counted per UTC month and resets at the month boundary; existing keys keep their current spend until the month ends (migration 008).
- **OpenAI-Compatible Streams Dropped Tool Calls, Reasoning and Usage:** the streaming parser for Groq, NVIDIA NIM, OpenRouter, Kilo and Cline read only text deltas, so a streamed tool call, reasoning text and token usage were lost. It now assembles tool calls from their streamed fragments, reports reasoning as thinking, reads usage from the final chunk (including Groq's `x_groq.usage` and usage sent after `finish_reason`), and reports a mid-stream error object as an error. Live streaming depends on this.
- **Key Names Unescaped in the Dashboard:** key names and IDs in the keys table were inserted as HTML; they are now escaped.
- **Rate Limits Could Lock Out a Key After a Clock Change:** a key's rate-limit buckets were created with the wall clock but checked with the enforcer's clock, and a clock that stepped backwards (for example after an NTP correction) refilled them by a negative amount, rejecting the key until the clock caught up. Buckets now start from the enforcer's clock and ignore backwards steps.
- **Browser Attacks on the Local Gateway:** the admin API has no authentication and the gateway applies provider keys itself, so a web page open in the user's browser could drive it. Three changes close this. When the gateway listens on a loopback address, requests whose `Host` is not a loopback name are refused, which stops DNS-rebinding pages from reading or driving the API (including `/v1/*`, which spends provider keys). Requests other than GET, HEAD and OPTIONS are refused when the browser marks them as cross-site (`Sec-Fetch-Site`, or an `Origin` that is not the gateway), which stops form posts from other sites; API clients, SDKs, curl and the CLI send neither header and are unaffected. The live-feed event stream no longer sends `Access-Control-Allow-Origin: *`, so other sites cannot read it. A gateway configured to listen on all interfaces skips the Host check.
- **Streamed Upstream Replies Reported as Cache Hits:** routed replies and emergency failovers sent to streaming clients were written through the cache-hit replay, which overwrote `X-Liltok-Cache-Status` with `HIT` and `X-Liltok-Provider` with `cache-local`, so clients and logs saw a cache hit for an upstream miss. They now keep their `MISS` headers and the serving provider.
- **Admin Pack Endpoint Could Write Anywhere:** `POST /api/v1/cache/pack` wrote the pack to any `target_path` in the request body. The admin API is unauthenticated and reachable from a browser on the same machine, so a web page could have used it to overwrite files. The path must now be relative to the working directory and end in `.json.gz`.
- **Unreadable Error When Every Target Was Skipped:** when no target in a chain could be tried (all too small for the prompt, inactive, excluded or with open breakers), the error read `all providers in fallback chain failed: %!w(<nil>)`; it now says every target was skipped.
- **Per-Key Token Limits Were Not Enforced (M9.2):** virtual keys' tokens-per-minute limit was charged 1 token per request, so a 100,000 TPM key allowed 100,000 requests a minute of any size. It is now charged with the request's estimated prompt size (body bytes / 4). A request larger than the whole per-minute budget is admitted when the bucket is full and drains it, instead of never fitting. Also fixed in the same code: the RPM and TPM buckets were updated by concurrent requests without a lock (a data race that could admit more requests than the limit), a request rejected for TPM still used up RPM, and a limit of 0 rejected every request instead of meaning no limit.
- **Model Prices Out of Date (M9.4):** the price table loaded from the database replaced the built-in one entirely, and its seed (from 0.1.x) priced every `claude-opus-5*` model, including Opus 5.5, at $15 / $75 per million tokens and Sonnet 5 at $3 / $15, had no Haiku 4.5 row, and matched `gpt-4o-mini` with the unanchored `gpt-4o` rule. Cost and savings figures for those models were overstated (about 3.75x for Opus 5.5). Prices now follow Anthropic's list rates: Opus 5.5 $4 / $20 (cache reads $0.20), Opus 5 and Opus 4.5-4.8 $5 / $25, Sonnet 5 $2 / $10, Sonnet 4.x $3 / $15, Haiku 4.5 $1 / $5, Fable 5.1 $10 / $50 (cache reads $0.25). Migration 007 corrects the seeded rows (only while they still hold their seed values, so edited rates are kept) and adds the missing models; database rows now take precedence over the built-in rules, longest pattern first, and the built-in rules cover any model the table does not list. Costs already recorded in `request_logs` are not recalculated.
- **Switching Strategy Could Corrupt liltok.yaml:** when the config had a `routes:` section without `default_strategy`, saving a strategy appended a second `routes:` block, and the next start failed with a duplicate-key YAML error. The key is now inserted into the existing section.
- **MCP Cache Search Crashed on Short Hashes:** the local SQLite fallback of `liltok_cache_search` sliced every hash to 12 characters, so a cache row with a shorter hash (possible in imported packs) panicked and stopped the MCP server.
- **Data Races and Nil Panics:** the OpenAI-compatible adapter's `StreamChat` read its base URL and API key without the adapter lock; miner progress events read shared counters after releasing their lock; the miner's semantic-cache store dereferenced a nil request when normalization failed; and a cache sync that downloaded a pack with no database attached panicked instead of returning an error.
- **Starter Seed Count Included Skipped Rows:** reseeding a database with fewer than five entries reported ignored duplicates as inserted.
- **Routes Endpoint Showed Stale Chains:** `GET /api/v1/routes` returned a hand-written copy of the routes that had drifted from the ones the router uses (it listed models no route contained and a different free-tier order), so the dashboard and `liltok route` showed chains that were not in effect. It now lists the router's live routes, adding `strategy`, structured `target_specs`, `built_in` and `customized` to each; the existing `targets` strings are unchanged. Route names, descriptions and models are HTML-escaped in the dashboard.

## [0.2.3-beta] - 2026-09-25

### Added
- **Model Catalog with Health from Upstream Replies (M8.3):** when a provider answers that a model does not exist or was retired (410, or a 404/400 whose body says the model is not found, does not exist, was decommissioned, or has no endpoints), that provider/model is marked inactive and fallback chains skip it without calling it. After `routes.model_recheck_hours` (`LILTOK_MODEL_RECHECK_HOURS`, default 24) one request re-checks it: success restores it, another "gone" reply restarts the wait. Health is stored in a new `model_catalog` table (migration 006), so it survives restarts. `GET /api/v1/models/catalog` lists inactive models with the reason and next recheck time, and `POST /api/v1/models/catalog/reactivate` restores one immediately. A 404 whose body is not about a model (for example a wrong base URL) and a 400 about the payload do not mark anything inactive.
- **CI Workflow:** `.github/workflows/ci.yml` runs on every push to `main` and on pull requests: gofmt check, `go vet`, golangci-lint, and the race-enabled test suite with a coverage floor (63%, the current total; raise it toward the 85% v1.0 target). The same checks run locally through new Makefile targets `fmt`, `fmt-check`, `vet`, `lint`, `cover` and `check`.
- **Lint Configuration:** `.golangci.yml` (golangci-lint v2, pinned to v2.14.0 in the Makefile) enables the standard linters with staticcheck's bug and simplification checks. Unchecked errors from read-side `Close` calls and in tests are allowed; write-side `Close` errors are checked. The codebase lints clean.
- **Cache-Hit Load Benchmarks (M10.3):** `BenchmarkCacheHitOpenAI` and `BenchmarkCacheHitAnthropic` in `internal/proxy` drive the real handlers against a warmed Tier-1 entry and report P50/P99 alongside ns/op; they fail if any iteration misses the cache or reaches the upstream. `scripts/k6/cache_hit.js` load-tests a running gateway at a constant arrival rate (default 5000 req/s for 30 s) with thresholds for P99 < 5 ms and a 99.9% Tier-1 hit rate; its setup warms the entry and aborts unless the next reply is a hit.

### Changed
- **Deprecated-Model Tables Removed:** the hand-maintained replacement tables for Groq, NVIDIA NIM, OpenRouter, Kilo and Cline (about 110 entries) and the NVIDIA NIM decommissioned list are gone; the model catalog now learns retired models from the provider's own reply. A request for a retired ID is no longer rewritten to a hand-picked replacement: its target is skipped (after at most one "not found" reply) and the rest of the chain serves it. Shorthand names remain as exact-match aliases: `auto` and `openrouter/auto` resolve to `openrouter/free`, and `auto`, `free`, `kilo-auto` and `kilo/...` forms to `kilo-auto/free`. The old tables also matched loosely, so a bare `free` was rewritten to a DeepSeek model on OpenRouter; it now resolves to `kilo-auto/free`.
- **Versioned Schema Migrations (M10.4):** the database schema is now built from numbered migrations recorded in a new `schema_migrations` table; each runs once, in its own transaction. Previously every start re-ran the whole init script, five `ALTER TABLE` statements with their errors discarded, and three backfill `UPDATE`s over the full request log. Databases created before this change are migrated in place: missing `request_logs` columns are added, the one-time backfills run once, and the `sync_metadata` table the cache syncer created on its own is now migration 005. New schema changes go in as the next numbered migration; `DB.SchemaVersion` reports the applied version.
- **Release Workflow:** the Go version now comes from `go.mod` instead of a hardcoded 1.24 (which only built through automatic toolchain download), and release tests run with the race detector.
- **Code Formatting:** all Go files are gofmt-clean (28 files reformatted, whitespace and alignment only).
- **Gemini 3.5 Flash-Lite as Last Resort:** replaces the full exclusion from 0.2.2-beta. Live Claude Code runs showed Flash-Lite stalling, looping and losing track in tool-heavy sessions, but fully excluding it left 30-110 s free-tier turns once the larger Gemini models hit quota. It now stays in the `auto-resilient` and `free-first` chains but is tried only after every other target has failed or been skipped. Two new settings control this for any provider: `routes.last_resort_models` (`LILTOK_LAST_RESORT_MODELS`, default `["gemini-3.5-flash-lite"]`) moves matching targets to the end of the chain, and `routes.excluded_models` (`LILTOK_EXCLUDED_MODELS`, default empty) never uses them; excluded models are also rejected when an auto-routing upstream (`openrouter/free`, `kilo-auto/free`) reports serving one. Entries match ignoring case, provider prefix and `:free`-style suffix. Configs without the keys get the defaults; an empty list clears a default.

### Fixed
- **Mined Cache Export Could Be Truncated Silently:** `liltok mine --export` ignored the error from closing the output file, so a failed flush left a truncated archive while reporting success. The close error is now returned.
- **Shutdown Flush Errors:** failures closing the cache store and request ledger on shutdown are logged instead of dropped.
- **Test Suite Timing Out Under the Race Detector:** every test that opened a temporary database decoded and inserted the 20 MB embedded starter pack, so `go test -race` timed out after 10 minutes in five packages. `LILTOK_SKIP_STARTER_SEED=1` now skips seeding (set by `make cover` and both workflows; the seeding test re-enables it), bringing the race-enabled suite to about 2.5 minutes.
- **MCP Messages Written in Two Parts:** each JSON-RPC response was written as the message and then a separate newline; it is now a single write per line.
- **Token Counting Rebuilt the Tokenizer on Every Call:** `tokens.CountTokens` built a new tiktoken encoder per call, sorting the full vocabulary each time. A Tier-1 cache hit took about 77 ms and allocated 14 MB, 82% of it spent building encoders. Encoders are now built once per encoding and shared (a failed download of the encoding data is retried at most once a minute). A hit now takes about 40 microseconds and 15 KB in the proxy benchmark.
- **Cache Hits Re-Tokenized the Prompt:** the non-streaming upstream paths stored cache entries without their token counts, so every hit counted the prompt tokens again. Entries now store the counts, and hits use them; entries written without counts are still counted on hit.
- **Request Ledger Dropped Records Under Load:** each audit record was written in its own transaction, so at about 4000 req/s the queue filled and roughly two thirds of records were dropped, each drop logging its own warning. Queued records are now written up to 256 per transaction, and the drop warning is logged at most every 5 s with a count. In a 30 s k6 run at 4900 req/s of cache hits, the ledger kept 146,932 of 147,207 records, and handler-side hit latency was P99 1 ms.
- **Seeded Pricing Overwritten on Every Start:** the init script's `INSERT OR REPLACE` of baseline model pricing ran at each startup, resetting any edited rows. It now runs once, when the schema is created.

## [0.2.2-beta] - 2026-09-25

### Added
- **Translation Replay Harness:** `internal/proxy/testdata/replay/*.json` fixtures pair a Claude Code `/v1/messages` request with scripted upstream replies per fallback attempt. Each runs through the real handler (pruner, router validators, translator, SSE replay) in JSON and streaming mode and checks the content blocks, stop reason, attempt count, and upstream request text. The first 13 fixtures cover the empty-turn, thinking-split, tool-name, text tool-call, repetition-loop, tool-output, plan-mode, and chain-exhaustion fixes from 0.1.9 to 0.2.1. `Router.SetRoute` registers a named route.
- **Failover Attempts in the Live Feed:** every failed attempt in a fallback chain (upstream error, timeout, 429, or a rejection by liltok's own checks such as empty turns, undeclared tools, stalls and loops) is broadcast as a `failover` event and shown in the dashboard's live feed with its provider, model, latency and error. Attempts are not written to `request_logs`, so request counts and hit rates still count client requests only. `router.AttemptResult` now carries each attempt's target, latency, raw reply and final error; fixture capture keeps the raw reply even when a check rejected it.
- **Replay Fixture Capture:** set `routes.capture_dir` (or `LILTOK_CAPTURE_DIR`) to record a fixture for every routed `/v1/messages` request answered by a translated provider. Each file holds the original request, the raw upstream reply of every fallback attempt, and expectations that snapshot the response sent. Correct the expectations and move the file into `internal/proxy/testdata/replay/` to turn a live failure into a regression test. Captures contain the full conversation, so the option is off by default and files are written owner-only. `router.WithAttemptObserver` exposes the raw attempts.

### Changed
- **Gemini 3.5 Flash-Lite Excluded:** removed from the `auto-resilient` and `free-first` chains after live Claude Code runs showed it stalling, looping and losing track in tool-heavy sessions. The new `routes.excluded_models` setting (`LILTOK_EXCLUDED_MODELS`, default `["gemini-3.5-flash-lite"]`) keeps listed models out of every provider: matching targets are skipped, IDs match ignoring case, provider prefix and `:free`-style suffix, and replies from auto-routing upstreams (`openrouter/free`, `kilo-auto/free`) that report an excluded model fail over. Configs without the key keep the default; `excluded_models: []` re-enables the model.
- **Miner Default Target Models:** mined entries are now stored under `claude-opus-5-5` and `claude-haiku-4-5-20251001` instead of `claude-opus-5` and `claude-haiku-4-5`. Cache keys hash the exact model string, and Claude Code sends the new IDs, so entries under the old IDs produced almost no hits. The CLI, dashboard, admin API, and corpus audit share one default list (`miner.DefaultTargetModels`). The old IDs remain selectable in the dashboard. The `liltok_ask` MCP tool now defaults to `claude-opus-5-5`.
- **Premium-Only Route:** targets `claude-opus-5-5` instead of `claude-opus-5`.

### Fixed
- **OpenAI-Format Requests Routed to Anthropic (M7.1):** the OpenAI request parser dropped assistant `tool_calls`, `tool_call_id`, `tool_choice` and array content, and the Anthropic adapter's translation dropped tools and sent `role: "tool"` messages that Anthropic rejects. Tool calls now become `tool_use` blocks, tool results become `tool_result` blocks merged into one user turn, OpenAI tool definitions and `tool_choice` are converted, mid-conversation system messages join `system`, and an explicit `temperature`/`top_p` of 0 is kept (new `HasTemperature`/`HasTopP` request flags). `tool_choice` now also reaches the OpenAI-compatible adapter.
- **Gemini Tool History:** tool results were sent to Gemini as plain user text rather than `functionResponse` parts, so Gemini saw its function calls answered by unstructured text. Tool results are now `functionResponse` parts matched to the earlier `functionCall` by name, consecutive same-role turns are merged, empty turns are dropped, and replayed `functionCall` parts carry Gemini's placeholder thought signature (`skip_thought_signature_validator`), which Gemini 3 requires once function history is structured (verified against the live API: 400 without it, 200 with it). In a live Claude Code run, a three-turn tool task dropped from 129 s (every Gemini attempt failing over) to 11 s served entirely by Gemini.
- **Token Usage in Streamed Replies:** routed replies replayed as Anthropic SSE reported `input_tokens: 0` and `output_tokens: 0`, so Claude Code could not track its context size (and would not auto-compact) while a free model was serving. The replay now reports the translated response's usage, falling back to the cache entry's counts.
- **ExitPlanMode Synthesized Outside Plan Mode:** a text answer that mentioned ExitPlanMode together with plan-like headers was turned into an `ExitPlanMode` call even when plan mode was not active; Claude Code rejected each one and the model retried. Synthesis now requires active plan mode.
- **Plan Mode Triggered by Tool Output:** plan-mode state was read from any message containing Claude Code's plan-file wording, including tool results. An agent reading a file that quotes that wording (such as liltok's own `planmode.go`) switched plan mode on, and every answer became a synthesized `ExitPlanMode` call until the run hit its turn limit. Plan mode is now read only from `<system-reminder>` blocks in user or system turns.
- **Stalled Agent Turns:** a short reply that announces its next tool action ("Let me read adapter.go.") without calling a tool ended Claude Code runs as if it were the final answer. In requests that declare tools, such turns now fail over to the next target. Closings such as "Let me know if..." are not affected.
- **Tool Calls Leaked as Text:** Gemini models sometimes write a call as `call:default_api:Grep{path:...,pattern:...}` in the answer text. It reached Claude Code as a final answer and ended agent runs early. Such calls are now converted to structured tool calls, splitting unquoted arguments only at the tool's declared parameter names; call syntax that cannot be converted triggers failover.
- **Anthropic Adapter Streaming (M7.2):** `StreamChat` reported every delta as text, so `thinking_delta` and tool-use `input_json_delta` events came through empty and tool names and IDs were lost; it also dropped usage, read the base URL without the adapter lock, and could block forever if the consumer stopped reading. It now emits `thinking_delta`, complete `tool_call` events assembled from the input fragments, a `finish` event with input, output and cached token counts, and stream `error` events as errors, and stops when the context is cancelled.

---

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
