param([string]$OpenSSL = 'C:\Program Files\Git\usr\bin\openssl.exe')
$ErrorActionPreference = 'Stop'
$taskRuntime = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../../runtime/backend-postgres'))
$taskTLS = Join-Path $taskRuntime 'tls'
New-Item -ItemType Directory -Force -Path $taskRuntime | Out-Null
$taskSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
& icacls $taskRuntime /inheritance:r /grant:r "*${taskSID}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Cannot restrict fixture directory permissions' }
New-Item -ItemType Directory -Force -Path $taskTLS | Out-Null

function Invoke-FixtureOpenSSL {
    param([string[]]$TaskArguments)
    & $OpenSSL @TaskArguments 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'OpenSSL fixture generation failed' }
}

Invoke-FixtureOpenSSL -TaskArguments @('req','-x509','-newkey','rsa:2048','-nodes','-sha256','-days','2','-subj','/CN=odysseia-test-ca','-keyout',"$taskTLS/ca.key",'-out',"$taskTLS/ca.crt")
Invoke-FixtureOpenSSL -TaskArguments @('req','-newkey','rsa:2048','-nodes','-sha256','-subj','/CN=localhost','-keyout',"$taskTLS/server.key",'-out',"$taskTLS/server.csr")
@'
subjectAltName=DNS:localhost,DNS:postgres
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
'@ | Set-Content -LiteralPath "$taskTLS/server.ext" -Encoding utf8NoBOM
Invoke-FixtureOpenSSL -TaskArguments @('x509','-req','-in',"$taskTLS/server.csr",'-CA',"$taskTLS/ca.crt",'-CAkey',"$taskTLS/ca.key",'-CAcreateserial','-days','2','-sha256','-extfile',"$taskTLS/server.ext",'-out',"$taskTLS/server.crt")
Invoke-FixtureOpenSSL -TaskArguments @('req','-x509','-newkey','rsa:2048','-nodes','-sha256','-days','2','-subj','/CN=untrusted-test-ca','-keyout',"$taskTLS/untrusted.key",'-out',"$taskTLS/untrusted.crt")

$taskAdminPassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24)).ToLowerInvariant()
$taskMigrationPassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24)).ToLowerInvariant()
$taskRuntimePassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24)).ToLowerInvariant()
$taskRealtimePassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24)).ToLowerInvariant()
$taskObserverPassword = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24)).ToLowerInvariant()
Set-Content -LiteralPath "$taskRuntime/admin-password" -Value $taskAdminPassword -NoNewline -Encoding utf8NoBOM
@"
CREATE ROLE odysseia_migrator LOGIN PASSWORD '$taskMigrationPassword' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE odysseia_runtime LOGIN PASSWORD '$taskRuntimePassword' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE odysseia_realtime LOGIN PASSWORD '$taskRealtimePassword' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE odysseia_observer LOGIN PASSWORD '$taskObserverPassword' NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE DATABASE odysseia_realtime_test OWNER odysseia_migrator;
REVOKE ALL ON DATABASE odysseia_realtime_test FROM PUBLIC;
GRANT CONNECT ON DATABASE odysseia_realtime_test TO odysseia_runtime, odysseia_realtime, odysseia_observer;
ALTER DATABASE odysseia_test OWNER TO odysseia_migrator;
REVOKE ALL ON DATABASE odysseia_test FROM PUBLIC;
GRANT CONNECT ON DATABASE odysseia_test TO odysseia_runtime;
"@ | Set-Content -LiteralPath "$taskRuntime/bootstrap.sql" -Encoding utf8NoBOM
@{
    kind = 'odysseia-isolated-postgres-test'
    migrationPassword = $taskMigrationPassword
    runtimePassword = $taskRuntimePassword
    realtimePassword = $taskRealtimePassword
    observerPassword = $taskObserverPassword
    caFile = "$taskTLS/ca.crt"
    untrustedCAFile = "$taskTLS/untrusted.crt"
} | ConvertTo-Json | Set-Content -LiteralPath "$taskRuntime/fixture.json" -Encoding utf8NoBOM
Write-Output 'Prepared synthetic PostgreSQL TLS fixture; secrets remain in the restricted runtime directory.'
