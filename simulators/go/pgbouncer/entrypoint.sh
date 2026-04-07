#!/bin/bash
set -e

# Generate userlist.txt with postgres credentials
cat > /etc/pgbouncer/userlist.txt <<EOF
"postgres" "postgres"
EOF

# In session mode, each backend is dedicated to one client for the full session.
# DEALLOCATE ALL drops named prepared statements (lc_0, lc_1, …) that pgx creates for
# query description caching, preventing cross-client statement name collisions.
#
# In transaction mode, backends are shared across many clients, but each transaction is
# independent and isolated. After COMMIT/ROLLBACK the backend is clean — no lingering
# transaction state, locks, or cursors. Prepared statements are connection-scoped and
# harmless to leave (they're actually beneficial as cache hits for repeated SQL).
# No reset query is needed; letting pgx reuse cached descriptions improves performance.
if [ "${POOL_MODE:-session}" = "session" ]; then
    RESOLVED_RESET_QUERY="DEALLOCATE ALL"
else
    RESOLVED_RESET_QUERY=""
fi

# Generate pgbouncer.ini from environment variables
cat > /etc/pgbouncer/pgbouncer.ini <<EOF
################## Auto generated ##################
[databases]
${DB_NAME:-simulator} = host=${DB_HOST:-postgres} port=${DB_PORT:-5432} auth_user=${DB_USER:-postgres}

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = ${LISTEN_PORT:-6432}
unix_socket_dir =
user = postgres
auth_file = /etc/pgbouncer/userlist.txt
auth_type = ${AUTH_TYPE:-plain}
pool_mode = ${POOL_MODE:-session}
max_client_conn = ${MAX_CLIENT_CONN:-100}
default_pool_size = ${DEFAULT_POOL_SIZE:-10}
min_pool_size = ${MIN_POOL_SIZE:-2}
ignore_startup_parameters = extra_float_digits
# max_prepared_statements = ${MAX_PREPARED_STATEMENTS:-0}

# Log settings
admin_users = postgres

# Connection sanity checks, timeouts
server_reset_query = ${RESOLVED_RESET_QUERY}

# TLS settings

# Dangerous timeouts
################## end file ##################
EOF

echo "Starting pgbouncer..."
exec pgbouncer /etc/pgbouncer/pgbouncer.ini
