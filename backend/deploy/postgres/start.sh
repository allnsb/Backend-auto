#!/bin/sh
set -eu
chown postgres:postgres /var/lib/postgresql
install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/tls
install -o postgres -g postgres -m 0644 /fixtures/server.crt /var/lib/postgresql/tls/server.crt
install -o postgres -g postgres -m 0600 /fixtures/server.key /var/lib/postgresql/tls/server.key
exec /usr/local/bin/docker-entrypoint.sh "$@"
