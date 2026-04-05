# Build locally to avoid registry pulls
# Based on edoburu/pgbouncer but with local caching
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends pgbouncer postgresql-client ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /etc/pgbouncer

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 6432

ENTRYPOINT ["/entrypoint.sh"]

