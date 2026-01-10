#!/usr/bin/env bash
set -euo pipefail

PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-postgres}"
PGPASSWORD="${PGPASSWORD:-postgres}"
PGDATABASE="${PGDATABASE:-postgres}"

PROXY_HOST="${PROXY_HOST:-127.0.0.1}"
PROXY_PORT="${PROXY_PORT:-5711}"
PROXY_START_TIMEOUT="${PROXY_START_TIMEOUT:-60}"

START_PROXY="${START_PROXY:-1}"

cleanup() {
  if [[ -n "${proxy_pid:-}" ]]; then
    kill "${proxy_pid}" >/dev/null 2>&1 || true
    wait "${proxy_pid}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [[ "${START_PROXY}" == "1" ]]; then
  export GRUNYAS_SERVER_LISTEN_ADDR="${PROXY_HOST}:${PROXY_PORT}"
  export GRUNYAS_SERVER_SSL_MODE="never"
  export GRUNYAS_SERVER_SSL_CERT=""
  export GRUNYAS_SERVER_SSL_KEY=""
  export GRUNYAS_AUTH_USERNAME="${PGUSER}"
  export GRUNYAS_AUTH_PASSWORD="${PGPASSWORD}"
  export GRUNYAS_BACKEND_HOST="${PGHOST}"
  export GRUNYAS_BACKEND_PORT="${PGPORT}"
  export GRUNYAS_BACKEND_USER="${PGUSER}"
  export GRUNYAS_BACKEND_PASSWORD="${PGPASSWORD}"
  export GRUNYAS_BACKEND_DATABASE="${PGDATABASE}"

  go run ./cmd -no-console >/tmp/grunyas-proxy.log 2>&1 &
  proxy_pid=$!

  # Wait for proxy to start listening.
  ready=0
  for _ in $(seq 1 $((PROXY_START_TIMEOUT * 10))); do
    if ! kill -0 "${proxy_pid}" >/dev/null 2>&1; then
      break
    fi
    if command -v nc >/dev/null 2>&1; then
      if nc -z "${PROXY_HOST}" "${PROXY_PORT}" >/dev/null 2>&1; then
        ready=1
        break
      fi
    else
      if (exec 3<>/dev/tcp/"${PROXY_HOST}"/"${PROXY_PORT}") >/dev/null 2>&1; then
        exec 3>&-
        ready=1
        break
      fi
    fi
    sleep 0.1
  done

  if [[ "${ready}" != "1" ]]; then
    echo "Proxy did not become ready on ${PROXY_HOST}:${PROXY_PORT}"
    if kill -0 "${proxy_pid}" >/dev/null 2>&1; then
      echo "Proxy process is still running but not listening yet."
    fi
    if [[ -f /tmp/grunyas-proxy.log ]]; then
      echo "---- proxy log ----"
      tail -n 200 /tmp/grunyas-proxy.log
      echo "-------------------"
    fi
    exit 1
  fi
fi

EXPECTED_ERROR_FILES=(
  "tests/sql/02_errors.sql"
)

is_expected_error_file() {
  local file="$1"
  for expected in "${EXPECTED_ERROR_FILES[@]}"; do
    if [[ "${file}" == "${expected}" ]]; then
      return 0
    fi
  done
  return 1
}

run_sql_file() {
  local file="$1"
  local output

  echo "Running ${file}..."
  if is_expected_error_file "${file}"; then
    output=$(
      PGPASSWORD="${PGPASSWORD}" psql \
        "host=${PROXY_HOST} port=${PROXY_PORT} user=${PGUSER} dbname=${PGDATABASE} sslmode=disable" \
        -X -q -v ON_ERROR_STOP=0 -v VERBOSITY=terse \
        -f "${file}" 2>&1
    ) || true
    echo "  OK (expected errors)"
    return 0
  fi

  if output=$(
    PGPASSWORD="${PGPASSWORD}" psql \
      "host=${PROXY_HOST} port=${PROXY_PORT} user=${PGUSER} dbname=${PGDATABASE} sslmode=disable" \
      -X -q -v ON_ERROR_STOP=1 -v VERBOSITY=terse \
      -f "${file}" 2>&1
  ); then
    echo "  OK"
    return 0
  fi

  echo "  FAIL"
  echo "${output}"
  return 1
}

for file in tests/sql/*.sql; do
  run_sql_file "${file}"
done
