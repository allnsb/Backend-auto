#!/bin/sh
set -eu
umask 077
cat > /data/valkey.conf <<'CONFIG'
bind 0.0.0.0
protected-mode yes
port 0
tls-port 6379
tls-cert-file /fixtures/server.crt
tls-key-file /fixtures/server.key
tls-ca-cert-file /fixtures/ca.crt
tls-auth-clients no
tls-protocols "TLSv1.3"
aclfile /fixtures/users.acl
save ""
appendonly no
dir /data
maxmemory 128mb
maxmemory-policy noeviction
CONFIG
exec valkey-server /data/valkey.conf
