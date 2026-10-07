param(
    [string]$Duration = "20s",
    [int]$MissRate = 200,
    [int]$HitRate = 2000,
    [string]$Latency = "50ms"
)
# End-to-end Liltok run: mock upstream -> direct baseline -> Liltok (with RSS/CPU monitor) -> report.
# Run from the repo root. Builds into scripts/bench/bin (never touches a running liltok.exe).
$ErrorActionPreference = 'Stop'
$R = "scripts/bench/results"
New-Item -ItemType Directory -Force $R, "scripts/bench/bin" | Out-Null
Remove-Item "$R/liltok-bench.db*" -ErrorAction SilentlyContinue
go build -o scripts/bench/bin/mockupstream.exe ./scripts/bench/mockupstream
go build -o scripts/bench/bin/liltok-bench.exe ./cmd/liltok
powershell -File scripts/bench/hostinfo.ps1

$mock = Start-Process scripts/bench/bin/mockupstream.exe -ArgumentList "-addr", "127.0.0.1:9999", "-latency", $Latency -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 1
try {
    k6 run --quiet -e GATEWAY=direct -e BASE_URL=http://127.0.0.1:9999 -e DURATION=$Duration -e MISS_RATE=$MissRate -e HIT_RATE=$HitRate scripts/bench/overhead.js
    Invoke-RestMethod -Method Post http://127.0.0.1:9999/reset | Out-Null

    $lt = Start-Process scripts/bench/bin/liltok-bench.exe -ArgumentList "start", "--config", "scripts/bench/configs/liltok.bench.yaml" -PassThru -WindowStyle Hidden
    try {
        $ready = $false
        for ($i = 0; $i -lt 30 -and -not $ready; $i++) {
            Start-Sleep -Milliseconds 500
            try { Invoke-WebRequest -UseBasicParsing http://127.0.0.1:8181/ -TimeoutSec 2 | Out-Null; $ready = $true } catch { if ($_.Exception.Response) { $ready = $true } }
        }
        if (-not $ready) { throw "liltok did not become ready on 127.0.0.1:8181" }
        $secs = 2 * [int]($Duration -replace '[^0-9]', '') + 5 + 60
        Remove-Item "$R/stop.flag" -ErrorAction SilentlyContinue; $mon = Start-Process powershell -ArgumentList "-File", "scripts/bench/monitor.ps1", "-ProcessName", "liltok-bench", "-OutFile", "$R/liltok.resources.json", "-DurationSeconds", $secs, "-StopFile", "$R/stop.flag" -PassThru -WindowStyle Hidden
        k6 run --quiet -e GATEWAY=liltok -e BASE_URL=http://127.0.0.1:8181 -e API_KEY=bench-key -e DURATION=$Duration -e MISS_RATE=$MissRate -e HIT_RATE=$HitRate scripts/bench/overhead.js
        Start-Sleep -Seconds 1
        New-Item -ItemType File -Force "$R/stop.flag" | Out-Null; $mon.WaitForExit(); Remove-Item "$R/stop.flag"
        Write-Host ("upstream calls seen by mock: " + (Invoke-RestMethod http://127.0.0.1:9999/stats).upstream_calls)
    } finally { Stop-Process -Id $lt.Id -Force -ErrorAction SilentlyContinue }
} finally { Stop-Process -Id $mock.Id -Force -ErrorAction SilentlyContinue }
