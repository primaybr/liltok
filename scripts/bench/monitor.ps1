param(
    [Parameter(Mandatory = $true)][string]$ProcessName,   # e.g. liltok-bench (no .exe)
    [Parameter(Mandatory = $true)][string]$OutFile,
    [int]$DurationSeconds = 80,
    [int]$IntervalMs = 250,
    [string]$StopFile = ""
)
# Samples peak RSS (working set) and CPU of the named process and writes JSON.
# cpu_percent_of_one_core: CPU seconds consumed / wall seconds * 100 (can exceed 100).
$ErrorActionPreference = 'Stop'
$p = Get-Process -Name $ProcessName | Select-Object -First 1
if (-not $p) { throw "process $ProcessName not found" }
$procId = $p.Id
$cpu0 = $p.TotalProcessorTime.TotalSeconds
$t0 = [DateTime]::UtcNow
$peak = 0L; $sum = 0L; $n = 0; $peakCpu = 0.0
$prevCpu = $cpu0; $prevT = $t0
$end = $t0.AddSeconds($DurationSeconds)
while ([DateTime]::UtcNow -lt $end) {
    if ($StopFile -and (Test-Path $StopFile)) { break }
    $q = Get-Process -Id $procId -ErrorAction SilentlyContinue
    if (-not $q) { break }
    $now = [DateTime]::UtcNow
    $cpu = $q.TotalProcessorTime.TotalSeconds
    $wall = ($now - $prevT).TotalSeconds
    if ($wall -gt 0) {
        $inst = ($cpu - $prevCpu) / $wall * 100
        if ($inst -gt $peakCpu) { $peakCpu = $inst }
    }
    $prevCpu = $cpu; $prevT = $now
    if ($q.WorkingSet64 -gt $peak) { $peak = $q.WorkingSet64 }
    $sum += $q.WorkingSet64; $n++
    Start-Sleep -Milliseconds $IntervalMs
}
$totalWall = ([DateTime]::UtcNow - $t0).TotalSeconds
$result = [ordered]@{
    process                  = $ProcessName
    pid                      = $procId
    samples                  = $n
    wall_seconds             = [math]::Round($totalWall, 1)
    peak_rss_mb              = [math]::Round($peak / 1MB, 1)
    avg_rss_mb               = if ($n) { [math]::Round(($sum / $n) / 1MB, 1) } else { 0 }
    cpu_seconds_total        = [math]::Round($prevCpu - $cpu0, 2)
    cpu_percent_avg_of_one_core = [math]::Round(($prevCpu - $cpu0) / $totalWall * 100, 1)
    cpu_percent_peak_of_one_core = [math]::Round($peakCpu, 1)
}
$result | ConvertTo-Json | Set-Content -Path $OutFile -Encoding utf8
