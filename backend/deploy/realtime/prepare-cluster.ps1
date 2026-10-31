param()
$ErrorActionPreference = 'Stop'
$taskRuntime = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../runtime/backend-realtime/cluster'))
New-Item -ItemType Directory -Force -Path $taskRuntime | Out-Null
$taskSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls $taskRuntime /inheritance:r /grant:r "*${taskSID}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Cannot restrict shared broker fixture permissions' }
$taskPassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant()
$taskHash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($taskPassword))).ToLowerInvariant()
$taskPrefix = 'odysseia_' + [Guid]::NewGuid().ToString('N')
Set-Content -LiteralPath "$taskRuntime/valkey-password" -Value $taskPassword -NoNewline -Encoding utf8NoBOM
@"
user default off
user odysseia_realtime on #$taskHash ~$taskPrefix* &$taskPrefix* +@read +@write +@pubsub +@scripting +@connection -@dangerous +info
"@ | Set-Content -LiteralPath "$taskRuntime/users.acl" -Encoding utf8NoBOM
@'
CREATE DATABASE odysseia_cluster_test OWNER odysseia_migrator;
REVOKE ALL ON DATABASE odysseia_cluster_test FROM PUBLIC;
GRANT CONNECT ON DATABASE odysseia_cluster_test TO odysseia_runtime, odysseia_realtime, odysseia_observer;
'@ | Set-Content -LiteralPath "$taskRuntime/bootstrap.sql" -Encoding utf8NoBOM
@{ password=$taskPassword; prefix=$taskPrefix } | ConvertTo-Json | Set-Content -LiteralPath "$taskRuntime/fixture.json" -Encoding utf8NoBOM
Write-Output 'Prepared isolated shared-broker credentials and cluster test database bootstrap.'
