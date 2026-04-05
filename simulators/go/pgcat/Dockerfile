FROM ghcr.io/postgresml/pgcat:latest

# entrypoint generates pgcat.toml from env vars at startup
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 6432

ENTRYPOINT ["/entrypoint.sh"]
