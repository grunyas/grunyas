#!/bin/sh
set -e

# Generate /etc/pgcat/pgcat.toml from environment variables at container startup.
# This allows docker-compose to pass all configuration via env vars, matching
# the same pattern used by Grunyas and PgBouncer.

DB_HOST="${DB_HOST:-postgres}"
DB_PORT="${DB_PORT:-5432}"
DB_USER="${DB_USER:-postgres}"
DB_PASSWORD="${DB_PASSWORD:-postgres}"
DB_NAME="${DB_NAME:-simulator}"
POOL_MODE="${POOL_MODE:-session}"
MAX_CLIENT_CONN="${MAX_CLIENT_CONN:-100}"
POOL_SIZE="${POOL_SIZE:-10}"
MIN_POOL_SIZE="${MIN_POOL_SIZE:-2}"

mkdir -p /etc/pgcat

cat > /etc/pgcat/pgcat.toml <<EOF
[general]
host = "0.0.0.0"
port = 6432
# Disable prepared statement caching so applications must wrap SQL PREPARE/EXECUTE
# in BEGIN/COMMIT — the same requirement as Grunyas. Keeps the comparison fair.
prepared_statements_cache_size = 0

[pools.${DB_NAME}]
pool_mode = "${POOL_MODE}"
default_role = "any"

[pools.${DB_NAME}.users.0]
username = "${DB_USER}"
password = "${DB_PASSWORD}"
pool_size = ${POOL_SIZE}
min_pool_size = ${MIN_POOL_SIZE}
max_client_conn = ${MAX_CLIENT_CONN}

[[pools.${DB_NAME}.shards]]
servers = [["${DB_HOST}", ${DB_PORT}, "primary"]]
database = "${DB_NAME}"
EOF

echo "pgcat config generated:"
cat /etc/pgcat/pgcat.toml

exec pgcat /etc/pgcat/pgcat.toml
