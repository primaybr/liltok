param([string]$Dir = "scripts/bench/results", [string]$OutFile = "scripts/bench/results/RESULTS.md")
# Builds a markdown table from results/*.json. Gateways with no result file are NOT RUN.
$host_ = Get-Content "$Dir/host.json" -Raw | ConvertFrom-Json
$direct = $null
if (Test-Path "$Dir/direct.json") { $direct = Get-Content "$Dir/direct.json" -Raw | ConvertFrom-Json }
function F($v) { if ($null -eq $v) { return "n/a" } return ("{0:N2}" -f [double]$v) }
$lines = @()
$lines += "# Benchmark results"
$lines += ""
$lines += "Host: $($host_.cpu_model), $($host_.cpu_physical_cores)C/$($host_.cpu_logical_cores)T, $($host_.ram_gb) GB RAM, $($host_.os)"
$lines += "Tools: $($host_.tools.go); $($host_.tools.k6)"
$lines += "Captured: $($host_.captured_utc)"
$lines += ""
$lines += "| Gateway | Status | Miss P50 ms | Miss P95 ms | Miss P99 ms | Added P50 ms | Added P99 ms | Miss rps | Hit P50 ms | Hit P95 ms | Hit P99 ms | Hit rps | Err rate | Peak RSS MB | Peak CPU % (1 core) |"
$lines += "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
foreach ($g in @("direct", "liltok", "litellm", "bifrost")) {
    $f = "$Dir/$g.json"
    if (-not (Test-Path $f)) {
        $lines += "| $g | NOT RUN |" + (" |" * 13)
        continue
    }
    $r = Get-Content $f -Raw | ConvertFrom-Json
    $res = "n/a"; $cpu = "n/a"
    if (Test-Path "$Dir/$g.resources.json") {
        $m = Get-Content "$Dir/$g.resources.json" -Raw | ConvertFrom-Json
        $res = F $m.peak_rss_mb; $cpu = F $m.cpu_percent_peak_of_one_core
    }
    $a50 = "n/a"; $a99 = "n/a"
    if ($direct -and $g -ne "direct") {
        $a50 = F ($r.miss.latency_ms.p50 - $direct.miss.latency_ms.p50)
        $a99 = F ($r.miss.latency_ms.p99 - $direct.miss.latency_ms.p99)
    }
    $hit = $r.cache_hit
    $err = [double]$r.miss.error_rate
    $lines += "| $g | RAN | $(F $r.miss.latency_ms.p50) | $(F $r.miss.latency_ms.p95) | $(F $r.miss.latency_ms.p99) | $a50 | $a99 | $(F $r.miss.throughput_rps) | $(F $hit.latency_ms.p50) | $(F $hit.latency_ms.p95) | $(F $hit.latency_ms.p99) | $(F $hit.throughput_rps) | $(F $err) | $res | $cpu |"
}
$lines | Set-Content -Path $OutFile -Encoding utf8
$lines
