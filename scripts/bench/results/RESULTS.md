# Benchmark results

Host: Intel(R) Core(TM) i7-8750H CPU @ 2.20GHz, 6C/12T, 31.8 GB RAM, Microsoft Windows 11 Home Single Language 10.0.26200
Tools: go version go1.27.0 windows/amd64; k6.exe v2.2.0 (commit/00a9a1b7f5, go1.26.5, windows/amd64)
Captured: 2026-10-07T16:09:17Z

| Gateway | Status | Miss P50 ms | Miss P95 ms | Miss P99 ms | Added P50 ms | Added P99 ms | Miss rps | Hit P50 ms | Hit P95 ms | Hit P99 ms | Hit rps | Err rate | Peak RSS MB | Peak CPU % (1 core) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| direct | RAN | 30.40 | 30.89 | 31.16 | n/a | n/a | 100.07 | 30.36 | 30.81 | 31.18 | 500.00 | 0.00 | n/a | n/a |
| liltok | RAN | 31.20 | 41.06 | 48.65 | 0.80 | 17.49 | 100.07 | 0.51 | 2.22 | 10.39 | 500.00 | 0.00 | 565.60 | 147.70 |
| litellm | NOT RUN | | | | | | | | | | | | | |
| bifrost | NOT RUN | | | | | | | | | | | | | |
