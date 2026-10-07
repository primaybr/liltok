param([string]$OutFile = "scripts/bench/results/host.json")
# Captures host specs automatically (no usernames or paths are recorded).
$cpu = Get-CimInstance Win32_Processor | Select-Object -First 1
$os = Get-CimInstance Win32_OperatingSystem
$tools = [ordered]@{}
$tools.go = (go version) -join ' '
$tools.k6 = ((k6 version) -join ' ')
$info = [ordered]@{
    captured_utc   = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ")
    os             = $os.Caption + " " + $os.Version
    cpu_model      = $cpu.Name.Trim()
    cpu_physical_cores = (Get-CimInstance Win32_Processor | Measure-Object NumberOfCores -Sum).Sum
    cpu_logical_cores  = (Get-CimInstance Win32_Processor | Measure-Object NumberOfLogicalProcessors -Sum).Sum
    ram_gb         = [math]::Round($os.TotalVisibleMemorySize / 1MB, 1)
    tools          = $tools
}
New-Item -ItemType Directory -Force -Path (Split-Path $OutFile) | Out-Null
$info | ConvertTo-Json -Depth 4 | Set-Content -Path $OutFile -Encoding utf8
