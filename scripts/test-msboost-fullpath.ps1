# Called only by the isolated-core Python harness; never controls an existing core.
param(
    [Parameter(Mandatory = $true)][int]$Socks5Port,
    [string]$GameAddress = '35.155.204.207',
    [int]$GamePort = 8585,
    [string]$ExpectedMode = 'HANDSHAKE_DEFAULT',
    [Parameter(Mandatory = $true)][string]$ExpectedNode,
    [Parameter(Mandatory = $true)][string]$ExpectedExit,
    [Parameter(Mandatory = $true)][string]$CorePath,
    [Parameter(Mandatory = $true)][string]$LauncherPath,
    [switch]$IncludeMenu
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$script:RealTestStage = 'initialization'
try {
[Console]::OutputEncoding = New-Object Text.UTF8Encoding($false)
$OutputEncoding = [Console]::OutputEncoding
if ($Socks5Port -lt 1 -or $Socks5Port -gt 65535) { throw 'Invalid isolated SOCKS port.' }
if ([string]::IsNullOrWhiteSpace($env:MIERU_CONFIG_JSON_FILE) -or
    -not (Test-Path -LiteralPath $env:MIERU_CONFIG_JSON_FILE -PathType Leaf) -or
    -not [string]::IsNullOrWhiteSpace($env:MIERU_CONFIG_FILE)) {
    throw 'An isolated JSON configuration environment is required.'
}
$script:Utf8 = New-Object Text.UTF8Encoding($false)
$script:RealTestStage = 'load_launcher_definitions'
$source = [IO.File]::ReadAllText($LauncherPath, $script:Utf8)
$marker = '#<MSBOOST_PS>'
$offset = $source.IndexOf($marker, [StringComparison]::Ordinal)
if ($offset -lt 0) { throw 'Embedded PowerShell marker is missing.' }
$tokens = $null; $parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseInput($source.Substring($offset + $marker.Length), [ref]$tokens, [ref]$parseErrors)
if (@($parseErrors).Count -ne 0) { throw 'The full-path launcher has a syntax error.' }
$definitions = @($ast.EndBlock.Statements | Where-Object {
    $_ -is [Management.Automation.Language.FunctionDefinitionAst]
} | ForEach-Object { $_.Extent.Text })
. ([ScriptBlock]::Create([string]::Join([Environment]::NewLine, $definitions)))
# Fixed v2 contract values; launcher initialization is never executed.
$script:FullPathProbePort = 20424
$script:FullPathWarmupTimeoutMs = 5000
$script:LatencySampleCount = 3
$script:LatencyTimeoutMs = 2000
$script:Root = Split-Path -Parent $env:MIERU_CONFIG_JSON_FILE
$script:ExePath = $CorePath
$script:StatePath = Join-Path $script:Root 'msboost-state.dat'
$script:LegacyStatePath = Join-Path $script:Root 'msboost-current-config.txt'
$script:ErrorLogPath = Join-Path $script:Root 'msboost-last-error.log'
$script:GameAddress = $GameAddress
$script:GamePort = $GamePort
$script:DefaultPort = 1080
$script:CurlPath = Join-Path $env:SystemRoot 'System32\curl.exe'
if (-not (Test-Path -LiteralPath $script:CurlPath -PathType Leaf)) { $script:CurlPath = Join-Path $script:Root 'curl.exe' }
$script:IpCheckUrl = 'https://api-ipv4.ip.sb/ip'
$script:State = [pscustomobject]@{ ConfigFile = (Split-Path -Leaf $env:MIERU_CONFIG_JSON_FILE); Socks5Port = $Socks5Port }

# This reads only the isolated configuration chosen by the child environment.
$script:RealTestStage = 'verify_isolated_config'
$config = Get-EffectiveConfig
if ($null -eq $config -or $config.socks5Port -ne $Socks5Port -or
    ($null -ne $config.rpcPort -and $config.rpcPort -ne 0)) {
    throw 'The core configuration does not match the isolated test port and disabled RPC.'
}
$profiles = @($config.profiles | Where-Object { $_.profileName -ceq $config.activeProfile })
if ($profiles.Count -ne 1 -or [string]$profiles[0].handshakeMode -cne $ExpectedMode) { throw 'The isolated handshake mode did not match.' }
$nodes = @($profiles[0].servers)
if ($nodes.Count -ne 1 -or [string]$nodes[0].ipAddress -cne $ExpectedNode -or
    -not [string]::IsNullOrWhiteSpace([string]$nodes[0].domainName)) {
    throw 'The isolated configuration does not match the explicitly authorized node.'
}

$script:RealTestStage = 'measure_full_path'
$result = Invoke-FullPathTcpSamples -Socks5Port $Socks5Port -GameAddress $GameAddress -GamePort $GamePort `
    -Count 3 -TimeoutMs 2000 -WarmupTimeoutMs 5000
$script:RealTestStage = 'prepare_report'
$hasher = [Security.Cryptography.SHA256]::Create()
try { $launcherHash = [BitConverter]::ToString($hasher.ComputeHash([IO.File]::ReadAllBytes($LauncherPath))).Replace('-', '') }
finally { $hasher.Dispose() }
$report = [ordered]@{
    PowerShell = $PSVersionTable.PSVersion.ToString()
    LauncherSHA256 = $launcherHash
    Node = $ExpectedNode
    ExpectedExit = $ExpectedExit
    Mode = $ExpectedMode
    IsolatedSocksPort = $Socks5Port
    ConfigIsolationVerified = $true
    RpcDisabled = $true
    GameAddress = $GameAddress
    GamePort = $GamePort
    Result = $result
    Display = (Format-LatencyResult $result -Compact)
}

if ($IncludeMenu) {
    $script:RealTestStage = 'measure_menu_page'
    $script:CapturedMenuLines = New-Object 'Collections.Generic.List[string]'
    $script:CapturedMenuWaits = 0
    # Only console capture and the return-key wait are substituted. All exit-IP,
    # config, entry-TCP and full-path query functions below remain original.
    function Write-Host { param($Object, $ForegroundColor, [switch]$NoNewline) [void]$script:CapturedMenuLines.Add([string]$Object) }
    function Wait-ForMenu { $script:CapturedMenuWaits++ }
    Check-IpAddress
    if ($script:CapturedMenuWaits -ne 1) {
        throw 'The real page did not complete its measurement and return path.'
    }
    $menuText = [string]::Join([Environment]::NewLine, $script:CapturedMenuLines)
    $gameMatch = [regex]::Match($menuText, '当前到游戏服务器延迟：\s*([0-9]+) ms')
    $relayMatch = [regex]::Match($menuText, '当前到中转服务器延迟：\s*([0-9]+) ms')
    $exitMatch = [regex]::Match($menuText, '当前出口 IPv4：\s*([0-9.]+)')
    $menuPassed = $gameMatch.Success -and $relayMatch.Success -and $exitMatch.Success -and
        $exitMatch.Groups[1].Value -ceq $ExpectedExit
    $report.Menu = [ordered]@{
        Text = $menuText
        ReturnCount = $script:CapturedMenuWaits
        QueryFunctionsOriginal = $true
        Passed = $menuPassed
    }
}

$script:RealTestStage = 'serialize_result'
$report.Passed = $result.SuccessCount -eq 3 -and (-not $IncludeMenu -or $report.Menu.Passed)
$report | ConvertTo-Json -Depth 10 -Compress
} catch {
    $allowedMessages = @(
        'Invalid isolated SOCKS port.',
        'An isolated JSON configuration environment is required.',
        'Embedded PowerShell marker is missing.',
        'The full-path launcher has a syntax error.',
        'The core configuration does not match the isolated test port and disabled RPC.',
        'The isolated handshake mode did not match.',
        'The isolated configuration does not match the explicitly authorized node.',
        'The real page did not complete its measurement and return path.'
    )
    $safeMessage = if ($allowedMessages -ccontains $_.Exception.Message) { $_.Exception.Message } else { 'The PowerShell harness failed; native exception details were suppressed.' }
    [pscustomobject]@{
        HarnessFailure = $true
        Stage = $script:RealTestStage
        SafeMessage = $safeMessage
        ErrorType = $_.Exception.GetType().FullName
        ScriptLine = $_.InvocationInfo.ScriptLineNumber
    } | ConvertTo-Json -Compress
    exit 1
}
