## Protocol split: downstream vs upstream

Short version: downstream speaks server protocol, upstream speaks client protocol.

- Downstream uses `pgproto3.Backend` because we are the server: parse frontend
  messages, handle SSL/auth, and send backend responses directly on the client
  socket.
- Upstream uses `pgxpool`/`pgconn` because we are the client: pooled connection
  lifecycle, TLS/startup/cancel, context-aware reads, and protocol state
  tracking. We only drop to `PgConn()` for raw Send/Receive when proxying.

Avoid instantiating a separate `pgproto3.Frontend` on the upstream connection;
it can desync `pgx`'s protocol state and poison the pool.
