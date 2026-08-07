<#
.SYNOPSIS
  Installs box.agent as a Windows Scheduled Task: gets the box-agent
  binary, registers a task that starts it at boot and restarts it if it
  exits, and starts it immediately.

.DESCRIPTION
  Run from an elevated (Administrator) PowerShell prompt. See
  docs/INSTALL.md for background and the manual steps this automates.

  Only a linux/amd64 release binary is published today, so this script
  first tries to download a Windows asset (box-agent-windows-amd64.exe,
  in case one exists by the time you run this) and, if that 404s, falls
  back to building from source - which requires `git` and `go` on PATH.

  The task runs as SYSTEM so it starts at boot without anyone logged in.
  The registration token ends up in the task's stored arguments, which -
  like the systemd unit on Linux - is only readable by Administrators on
  this machine.

.EXAMPLE
  .\install.ps1 -Router wss://router.example.com -Provider yourns/your-model `
    -Token $env:REGISTRATION_TOKEN -ApiUrl https://api.example.com `
    -ApiToken $env:REGISTRATION_TOKEN -LlmUrl http://localhost:11434
#>
[CmdletBinding()]
param(
    [string]$Router = "wss://router.example.com",
    [Parameter(Mandatory = $true)][string]$Provider,
    [Parameter(Mandatory = $true)][string]$Token,
    [string]$ApiUrl = "https://api.example.com",
    [Parameter(Mandatory = $true)][string]$ApiToken,
    [string]$LlmUrl = "http://localhost:11434",
    [string]$LlmApiKey,
    [string]$BackendModel,
    [int]$ContextLength = 0,
    [int]$MaxOutputLength = 0,
    [string]$Quantization,
    [string]$InputModalities,
    [string]$OutputModalities,
    [string]$SupportedFeatures,
    [string]$Version = "latest",
    [string]$InstallDir = "$env:ProgramData\box-agent",
    [string]$TaskName = "box-agent"
)

$ErrorActionPreference = "Stop"

$currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "This script must be run from an elevated (Administrator) PowerShell prompt."
    exit 1
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exePath = Join-Path $InstallDir "box-agent.exe"
$repo = "LLMOcean/box.agent"
$asset = "box-agent-windows-amd64.exe"

if ($Version -eq "latest") {
    $url = "https://github.com/$repo/releases/latest/download/$asset"
} else {
    $url = "https://github.com/$repo/releases/download/$Version/$asset"
}

Write-Host "Downloading $url -> $exePath"
try {
    Invoke-WebRequest -Uri $url -OutFile $exePath -UseBasicParsing
} catch {
    Write-Warning "No published Windows binary found ($($_.Exception.Message)); building from source instead."

    if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
        throw "git not found on PATH; needed to fetch source for the fallback build."
    }
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "Go toolchain not found on PATH (install from https://go.dev/dl/); needed for the fallback build."
    }

    $srcDir = Join-Path $env:TEMP "box-agent-src"
    if (Test-Path $srcDir) { Remove-Item -Recurse -Force $srcDir }

    if ($Version -eq "latest") {
        git clone --depth 1 "https://github.com/$repo.git" $srcDir
    } else {
        git clone --depth 1 --branch $Version "https://github.com/$repo.git" $srcDir
    }

    Push-Location $srcDir
    try {
        go build -o $exePath .
    } finally {
        Pop-Location
    }
}

$argList = @("-router", $Router, "-provider", $Provider, "-api-url", $ApiUrl, "-llm-url", $LlmUrl)
if ($BackendModel) { $argList += @("-backend-model", $BackendModel) }
if ($ContextLength -gt 0) { $argList += @("-context-length", $ContextLength) }
if ($MaxOutputLength -gt 0) { $argList += @("-max-output-length", $MaxOutputLength) }
if ($Quantization) { $argList += @("-quantization", $Quantization) }
if ($InputModalities) { $argList += @("-input-modalities", $InputModalities) }
if ($OutputModalities) { $argList += @("-output-modalities", $OutputModalities) }
if ($SupportedFeatures) { $argList += @("-supported-features", $SupportedFeatures) }
$argList += @("-token", $Token, "-api-token", $ApiToken)
if ($LlmApiKey) { $argList += @("-llm-api-key", $LlmApiKey) }

# box-agent logs to stdout; a scheduled task has nowhere to send that
# unless we redirect it ourselves via a cmd.exe wrapper.
$logPath = Join-Path $InstallDir "box-agent.log"
$quotedArgs = ($argList | ForEach-Object { '"' + $_ + '"' }) -join ' '
$cmdLine = "/c `"`"$exePath`" $quotedArgs >> `"$logPath`" 2>&1`""

$taskAction = New-ScheduledTaskAction -Execute "cmd.exe" -Argument $cmdLine -WorkingDirectory $InstallDir
$taskTrigger = New-ScheduledTaskTrigger -AtStartup
$taskSettings = New-ScheduledTaskSettingsSet `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
$taskPrincipal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest

if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Write-Host "Task '$TaskName' already exists; replacing it."
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
}

Register-ScheduledTask -TaskName $TaskName -Action $taskAction -Trigger $taskTrigger `
    -Settings $taskSettings -Principal $taskPrincipal `
    -Description "router.agent remote WebSocket agent (box.agent)" | Out-Null

Start-ScheduledTask -TaskName $TaskName

Write-Host "box-agent installed and started as scheduled task '$TaskName'."
Write-Host "Logs: $logPath"
Write-Host "Status:  Get-ScheduledTask -TaskName $TaskName | Get-ScheduledTaskInfo"
Write-Host "Stop:    Stop-ScheduledTask -TaskName $TaskName"
Write-Host "Restart: Start-ScheduledTask -TaskName $TaskName"
