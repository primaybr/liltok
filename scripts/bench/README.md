# Gateway overhead benchmark

Head-to-head harness for Liltok, LiteLLM and Bifrost on one host against one deterministic
mock upstream, so results isolate gateway overhead.

## Layout

| File | Purpose |
|---|---|
| `mockupstream/main.go` | OpenAI-compatible mock (`/v1/chat/completions`), fixed `-latency`, `/stats` counts upstream calls |
| `overhead.js` | k6 script: `miss` scenario (unique prompts) then `cache_hit` scenario (fixed 200-prompt corpus) |
| `monitor.ps1` | Samples peak RSS and CPU of a process, writes JSON |
| `hostinfo.ps1` | Captures CPU, cores, RAM, OS, tool versions to `results/host.json` |
| `report.ps1` | Merges `results/*.json` into `results/RESULTS.md` (missing gateway = NOT RUN) |
| `run_liltok.ps1` | End-to-end Liltok run (mock, direct baseline, Liltok, monitor) |
| `configs/` | `liltok.bench.yaml`, `litellm.bench.yaml`, `bifrost.bench.json` |

Ports: mock 9999, Liltok 8181, LiteLLM 4000, Bifrost 8080 (set via its `-port` flag; use 8182 if 8080 is busy).

## Prerequisites

Go, k6, PowerShell. Run everything from the repo root. Binaries go to `scripts/bench/bin/`
and results to `scripts/bench/results/` (do not commit them).

## Liltok (wired up)

```
powershell -File scripts/bench/run_liltok.ps1 -Duration 20s -MissRate 200 -HitRate 2000 -Latency 50ms
powershell -File scripts/bench/report.ps1
```

Or by hand:

```
go build -o scripts/bench/bin/mockupstream.exe ./scripts/bench/mockupstream
go build -o scripts/bench/bin/liltok-bench.exe ./cmd/liltok
scripts/bench/bin/mockupstream.exe -addr 127.0.0.1:9999 -latency 50ms
k6 run -e GATEWAY=direct -e BASE_URL=http://127.0.0.1:9999 scripts/bench/overhead.js
scripts/bench/bin/liltok-bench.exe start --config scripts/bench/configs/liltok.bench.yaml
k6 run -e GATEWAY=liltok -e BASE_URL=http://127.0.0.1:8181 -e API_KEY=bench-key scripts/bench/overhead.js
```

The bench config has a single provider (the mock), no secrets, the maintainer disabled, and
its own database under `results/`.

## LiteLLM and Bifrost (configs only)

```
pip install "litellm[proxy]"
litellm --config scripts/bench/configs/litellm.bench.yaml --port 4000
k6 run -e GATEWAY=litellm -e BASE_URL=http://127.0.0.1:4000 -e API_KEY=bench-key scripts/bench/overhead.js

npx -y @maximhq/bifrost -app-dir scripts/bench/bifrost-data -port 8182
# copy configs/bifrost.bench.json to scripts/bench/bifrost-data/config.json first
k6 run -e GATEWAY=bifrost -e BASE_URL=http://127.0.0.1:8182 -e API_KEY=bench-key scripts/bench/overhead.js
```

Bifrost routes by `provider/model`; if it rejects `gpt-4o-mini`, pass `-e MODEL=openai/gpt-4o-mini`.
Record each gateway with the monitor while k6 runs:

```
powershell -File scripts/bench/monitor.ps1 -ProcessName <name> -OutFile scripts/bench/results/<gateway>.resources.json -DurationSeconds 80
```

Config schemas for LiteLLM and Bifrost were written from documentation memory and have not been validated here.

## Method and fairness rules

- One host, one mock, gateways run one at a time (never concurrently).
- `direct` = k6 straight to the mock. Added latency = gateway miss latency minus direct miss latency.
- Miss scenario uses a unique prompt per request, so no cache can serve it.
- Cache-hit scenario: setup() sends the 200 corpus prompts once; the load phase then cycles the corpus. Hit rate is not verified inside k6 for non-Liltok gateways; check the mock's `/stats` (`upstream_calls`) to confirm hits never reached upstream.
- Enable only exact-match caching for the hit scenario in every gateway.
- Warm up and repeat at least 3 times before quoting numbers; k6 runs on the same host so it competes for CPU.

## Output

`results/<gateway>.json` (k6 summary), `results/<gateway>.resources.json` (peak RSS, CPU), `results/host.json`,
`results/RESULTS.md` (table; template columns are in `report.ps1`).
