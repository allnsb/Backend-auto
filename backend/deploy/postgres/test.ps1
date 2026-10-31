param(
    [string]$GoExecutable = 'go',
    [string]$TestTags = 'integration',
    [string]$CentrifugoExecutable = '',
    [ValidateSet('baseline','mixed','fanout','discussions')][string]$PerformanceProfile = 'baseline',
    [ValidateSet('memory','disk')][string]$DatabaseStorage = 'memory',
    [switch]$VerboseTests
)
$ErrorActionPreference = 'Stop'
$taskCompose = Join-Path $PSScriptRoot 'compose.yaml'
$taskRuntime = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../runtime/backend-postgres'))
$taskBackend = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$taskProject = 'odysseia-backend-test'
$taskTags = $TestTags -split ','
$taskCluster = $taskTags -contains 'cluster'
$taskAcceptance = $taskTags -contains 'acceptance'
$taskPerformance = $taskTags -contains 'performance'
if (($taskTags -contains 'realtime') -and -not ($taskTags -contains 'integration')) { throw 'Realtime tests require integration,realtime tags.' }
if ($taskCluster -and -not ($taskTags -contains 'realtime')) { throw 'Cluster tests require integration,realtime,cluster tags.' }
if ($taskAcceptance -and -not $taskCluster) { throw 'Acceptance tests require integration,realtime,cluster,acceptance tags.' }
if ($taskPerformance -and -not ($taskTags -contains 'integration')) { throw 'Performance tests require integration,performance tags.' }
if ($PerformanceProfile -ne 'baseline' -and -not $taskPerformance) { throw 'A performance profile requires integration,performance tags.' }
if ($PerformanceProfile -eq 'fanout' -and -not ($taskTags -contains 'realtime')) { throw 'The fanout profile requires integration,realtime,performance tags.' }
if ($PerformanceProfile -eq 'discussions' -and -not ($taskTags -contains 'realtime')) { throw 'The discussions profile requires integration,realtime,performance tags.' }
$taskComposeArgs = @('-p', $taskProject, '-f', $taskCompose)
if ($DatabaseStorage -eq 'disk') { $taskComposeArgs += @('-f', (Join-Path $PSScriptRoot 'compose.disk.yaml')) }
$env:ODYSSEIA_TEST_DATABASE_STORAGE = $DatabaseStorage
if ($taskCluster) { $taskComposeArgs += @('-f', (Join-Path $taskBackend 'deploy/realtime/compose.cluster.yaml')) }
$taskExisting = & docker ps -aq --filter "label=com.docker.compose.project=$taskProject"
if ($LASTEXITCODE -ne 0) { throw 'Docker engine is unavailable' }
if ($taskExisting) { throw 'The dedicated test project already exists; stop that project before running this fixture.' }
if ($DatabaseStorage -eq 'disk') {
    $taskExistingVolumes = & docker volume ls -q --filter "label=com.docker.compose.project=$taskProject"
    if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect dedicated test-project volumes' }
    if ($taskExistingVolumes) { throw 'The dedicated test project has existing volumes; choose a clean fixture before running a disk profile.' }
}

if (($TestTags -split ',') -contains 'realtime') {
    if (-not $CentrifugoExecutable) { $CentrifugoExecutable = Join-Path $taskBackend '../runtime/backend/tools/centrifugo-6.10.2/centrifugo.exe' }
    if (-not (Test-Path -LiteralPath $CentrifugoExecutable -PathType Leaf)) { throw 'Provide the verified Centrifugo 6.10.2 executable for realtime tests.' }
    $env:ODYSSEIA_TEST_CENTRIFUGO_BINARY = [IO.Path]::GetFullPath($CentrifugoExecutable)
    $taskRealtimeRuntime = [IO.Path]::GetFullPath((Join-Path $taskBackend '../runtime/backend-realtime'))
    New-Item -ItemType Directory -Force -Path $taskRealtimeRuntime | Out-Null
    $taskSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    & icacls $taskRealtimeRuntime /inheritance:r /grant:r "*${taskSID}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Cannot restrict realtime fixture permissions' }
    $env:ODYSSEIA_TEST_REALTIME_RUNTIME = $taskRealtimeRuntime
}

& (Join-Path $PSScriptRoot 'prepare.ps1')
if ($taskCluster) { & (Join-Path $taskBackend 'deploy/realtime/prepare-cluster.ps1') }
$taskTestExit = 1
try {
    & docker compose @taskComposeArgs up -d --wait --wait-timeout 120
    if ($LASTEXITCODE -ne 0) { throw 'PostgreSQL fixture did not become healthy' }
    $taskAddress = & docker compose @taskComposeArgs port postgres 5432
    if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve the fixture port' }
    $taskPort = ([Uri]("tcp://" + $taskAddress.Trim())).Port
    $taskFixture = Get-Content -Raw -LiteralPath "$taskRuntime/fixture.json" | ConvertFrom-Json
    $taskCA = [Uri]::EscapeDataString($taskFixture.caFile)
    $env:ODYSSEIA_TEST_DATABASE_URL = "postgresql://odysseia_runtime:$($taskFixture.runtimePassword)@localhost:${taskPort}/odysseia_test?sslmode=verify-full&sslrootcert=$taskCA"
    $env:ODYSSEIA_TEST_MIGRATION_DATABASE_URL = "postgresql://odysseia_migrator:$($taskFixture.migrationPassword)@localhost:${taskPort}/odysseia_test?sslmode=verify-full&sslrootcert=$taskCA"
    $env:ODYSSEIA_TEST_UNTRUSTED_CA = $taskFixture.untrustedCAFile
    $env:ODYSSEIA_TEST_REALTIME_RUNTIME_URL = "postgresql://odysseia_runtime:$($taskFixture.runtimePassword)@localhost:${taskPort}/odysseia_realtime_test?sslmode=verify-full&sslrootcert=$taskCA"
    $env:ODYSSEIA_TEST_REALTIME_MIGRATION_URL = "postgresql://odysseia_migrator:$($taskFixture.migrationPassword)@localhost:${taskPort}/odysseia_realtime_test?sslmode=verify-full&sslrootcert=$taskCA"
    $env:ODYSSEIA_TEST_REALTIME_CONSUMER_URL = "postgresql://odysseia_realtime:$($taskFixture.realtimePassword)@localhost:${taskPort}/odysseia_realtime_test?sslmode=verify-full&sslrootcert=$taskCA"
    $env:ODYSSEIA_TEST_REALTIME_OBSERVER_URL = "postgresql://odysseia_observer:$($taskFixture.observerPassword)@localhost:${taskPort}/odysseia_realtime_test?sslmode=verify-full&sslrootcert=$taskCA"
    $env:ODYSSEIA_TEST_REALTIME_TLS_DIR = Join-Path $taskRuntime 'tls'
    if ($taskCluster) {
        $taskBroker = Get-Content -Raw -LiteralPath (Join-Path $taskBackend '../runtime/backend-realtime/cluster/fixture.json') | ConvertFrom-Json
        $taskValkeyAddress = & docker compose @taskComposeArgs port valkey 6379
        if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve shared broker port' }
        $taskValkeyPort = ([Uri]("tcp://" + $taskValkeyAddress.Trim())).Port
        $env:ODYSSEIA_TEST_VALKEY_ADDRESS = "localhost:$taskValkeyPort"
        $env:ODYSSEIA_TEST_VALKEY_PASSWORD = $taskBroker.password
        $env:ODYSSEIA_TEST_VALKEY_PREFIX = $taskBroker.prefix
        $env:ODYSSEIA_TEST_CLUSTER_RUNTIME_URL = "postgresql://odysseia_runtime:$($taskFixture.runtimePassword)@localhost:${taskPort}/odysseia_cluster_test?sslmode=verify-full&sslrootcert=$taskCA"
        $env:ODYSSEIA_TEST_CLUSTER_MIGRATION_URL = "postgresql://odysseia_migrator:$($taskFixture.migrationPassword)@localhost:${taskPort}/odysseia_cluster_test?sslmode=verify-full&sslrootcert=$taskCA"
        $env:ODYSSEIA_TEST_CLUSTER_CONSUMER_URL = "postgresql://odysseia_realtime:$($taskFixture.realtimePassword)@localhost:${taskPort}/odysseia_cluster_test?sslmode=verify-full&sslrootcert=$taskCA"
        $env:ODYSSEIA_TEST_CLUSTER_OBSERVER_URL = "postgresql://odysseia_observer:$($taskFixture.observerPassword)@localhost:${taskPort}/odysseia_cluster_test?sslmode=verify-full&sslrootcert=$taskCA"
    }
    Push-Location $taskBackend
    try {
        if ($taskAcceptance) {
            $taskStatusBinary = Join-Path $taskRealtimeRuntime 'realtime-status.exe'
            & $GoExecutable build -o $taskStatusBinary ./cmd/realtime-status
            if ($LASTEXITCODE -ne 0) { throw 'Cannot build realtime status command' }
            $env:ODYSSEIA_TEST_STATUS_BINARY = $taskStatusBinary
        }
        $taskTimeout = if ($taskPerformance -and $PerformanceProfile -in @('fanout','discussions')) { '420s' } elseif ($taskPerformance -and $PerformanceProfile -eq 'mixed') { '240s' } elseif ($taskAcceptance -or $taskPerformance) { '180s' } else { '60s' }
        $taskTestArgs=@('test',"-tags=$TestTags",'-count=1',"-timeout=$taskTimeout")
        if ($VerboseTests -and -not $taskPerformance) { $taskTestArgs += '-v' }
        if ($taskPerformance) {
            $taskProfileTest=switch ($PerformanceProfile) { 'mixed' { 'TestPerformanceMixedLoad' } 'fanout' { 'TestPerformanceHotTopicsFanout' } 'discussions' { 'TestPerformanceDiscussionModes' } default { 'TestPerformanceBaseline' } }
            $taskProfileOutput=switch ($PerformanceProfile) { 'mixed' { 'performance-mixed.json' } 'fanout' { 'performance-fanout.json' } 'discussions' { 'performance-discussions.json' } default { 'performance.json' } }
            $env:ODYSSEIA_TEST_PERFORMANCE_OUTPUT=Join-Path $taskRuntime $taskProfileOutput
            $taskTestArgs+=@("-run=^$taskProfileTest$",'-v')
        }
        $taskTestPackage=if ($taskPerformance) { './internal/postgres' } else { './...' }
        & $GoExecutable @taskTestArgs $taskTestPackage
        $taskTestExit = $LASTEXITCODE
    } finally {
        Pop-Location
    }
} finally {
    & docker compose @taskComposeArgs down --timeout 5 --volumes
    if ($LASTEXITCODE -ne 0) { throw 'Dedicated test-project cleanup failed' }
}
exit $taskTestExit
