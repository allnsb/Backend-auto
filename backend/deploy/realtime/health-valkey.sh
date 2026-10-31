#!/bin/sh
set -eu
VALKEYCLI_AUTH="$(cat /run/secrets/valkey_password)"
export VALKEYCLI_AUTH
exec valkey-cli --tls --cacert /fixtures/ca.crt --sni localhost -h localhost --user odysseia_realtime ping
