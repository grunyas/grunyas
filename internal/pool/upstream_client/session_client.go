package upstream_client

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SessionClient struct {
	conn       *pgxpool.Conn
	discardAll bool // run DISCARD ALL on release; false for transaction mode
}

func Initialize(conn *pgxpool.Conn, discardAll bool) *SessionClient {
	return &SessionClient{
		conn:       conn,
		discardAll: discardAll,
	}
}

func (s *SessionClient) TxStatus() byte {
	return s.conn.Conn().PgConn().TxStatus()
}

// Send buffers messages for delivery to the backend using pgx's internal
// pgproto3.Frontend. No syscall happens here — data accumulates in the
// Frontend's write buffer. Callers MUST call Flush to actually transmit.
//
// This replaces the previous approach that called msg.Encode(nil) (allocating
// a new []byte per message) and then conn.Write (one syscall per message).
// Using Frontend.Send reuses an internal wbuf and batches writes, eliminating
// both the per-message allocation and the per-message syscall.
//
// Safety: pgx's Frontend is normally accessed through pgx's high-level query
// API, which locks the connection. We're bypassing that lock, which is safe
// because:
//  1. We own this *pgxpool.Conn exclusively for the session's lifetime.
//  2. pgx is not doing any concurrent work on this connection — we never call
//     pgx.Query/Exec on it. pgx only sees the connection in a "clean" state.
//  3. net.Conn.Read and net.Conn.Write are safe to call concurrently from
//     different goroutines (Go stdlib guarantee), which is all the read loop
//     and main loop ever do on this connection.
//  4. Frontend.wbuf (write) and Frontend.cr (read) are independent buffers,
//     so our write-side usage does not race with pgConn.ReceiveMessage's
//     read-side usage.
func (s *SessionClient) Send(msgs ...pgproto3.FrontendMessage) error {
	frontend := s.conn.Conn().PgConn().Frontend()
	for _, msg := range msgs {
		frontend.Send(msg)
	}
	// Frontend.Send is infallible and defers any encode error to Flush, so
	// the returned error here is always nil. Encoding errors surface on the
	// next Flush call.
	return nil
}

// Flush writes any buffered messages to the backend connection. This is where
// the write syscall happens. Call at protocol boundaries (Sync, Query, Flush,
// CopyDone, CopyFail) so the backend can process the batched request.
func (s *SessionClient) Flush() error {
	return s.conn.Conn().PgConn().Frontend().Flush()
}

func (s *SessionClient) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	return s.conn.Conn().PgConn().ReceiveMessage(ctx)
}

// Release resets the connection state and returns it to the pool.
// It executes DISCARD ALL to ensure a clean state for the next consumer.
//
// NOTE: This is safe because Release() is only called from session.Close()
// AFTER wg.Wait() ensures the upstreamReadLoop goroutine has exited.
func (s *SessionClient) Release() error {
	if err := s.reset(); err != nil {
		return err
	}

	s.conn.Release()

	return nil
}

// Kill destroys the connection instead of returning it to the pool.
func (s *SessionClient) Kill() error {
	return s.conn.Hijack().Close(context.Background())
}

// reset executes DISCARD ALL to clear any session-level state.
func (s *SessionClient) reset() error {
	// Use a fresh context since the session context is already cancelled
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Send Sync unconditionally to flush any pending extended-query protocol
	// pipeline. If the session was interrupted mid-query (e.g., context expired
	// after Parse+Bind+Execute but before Sync), the backend is still waiting
	// for Sync to send its response. Without this, the subsequent drainToReady
	// would block until the 5s timeout. On an already-idle connection, Sync is
	// a no-op: the backend immediately responds with ReadyForQuery.
	//
	// We intentionally ignore send/flush errors here: if the backend connection
	// is broken, drainToReady will fail with an error and we'll kill it.
	_ = s.Send(&pgproto3.Sync{})
	_ = s.Flush()

	// Always drain pending backend messages up to ReadyForQuery. This handles
	// two cases:
	//  1. Sync triggered a ReadyForQuery (above) — consume it.
	//  2. The read loop left a stale ReadyForQuery in pgx's internal read buffer
	//     (from a previous response that was never forwarded). If we skip drain
	//     and send DISCARD ALL directly, simpleQuery would consume the stale
	//     ReadyForQuery as its own response and return early — DISCARD ALL would
	//     never actually execute, leaving prepared statements intact for the next
	//     session.
	if err := s.drainToReady(ctx); err != nil {
		return fmt.Errorf("drain pending messages: %w", err)
	}

	// If still not idle after draining (e.g., Sync was rejected inside an
	// aborted transaction), attempt a rollback.
	if s.TxStatus() != 'I' {
		if err := s.simpleQuery(ctx, "ROLLBACK"); err != nil {
			return err
		}
	}

	if s.discardAll {
		if err := s.simpleQuery(ctx, "DISCARD ALL"); err != nil {
			return err
		}
	}

	return nil
}

func (s *SessionClient) simpleQuery(ctx context.Context, query string) error {
	if err := s.Send(&pgproto3.Query{String: query}); err != nil {
		return fmt.Errorf("send %s: %w", query, err)
	}
	// Flush synchronously — simpleQuery is used for connection reset
	// (ROLLBACK / DISCARD ALL) where we must wait for the backend to
	// respond before releasing the connection to the pool.
	if err := s.Flush(); err != nil {
		return fmt.Errorf("flush %s: %w", query, err)
	}

	// Consume response: expect CommandComplete + ReadyForQuery.
	//
	// We require CommandComplete to appear BEFORE we accept ReadyForQuery as
	// the terminal response. This guards against stale ReadyForQuery messages
	// that may remain in pgx's receive buffer from sessions that ended before
	// all backend responses were consumed (e.g., the session context expired
	// while the upstream read loop was forwarding a query response, leaving a
	// ReadyForQuery unconsumed). Without this guard, simpleQuery would return
	// early on a stale ReadyForQuery, causing ROLLBACK or DISCARD ALL to never
	// actually execute on the backend.
	commandCompleteSeen := false
	var queryErr error
	for {
		msg, err := s.Receive(ctx)
		if err != nil {
			return fmt.Errorf("receive %s response: %w", query, err)
		}

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			if commandCompleteSeen {
				return queryErr
			}
			// Stale ReadyForQuery from a previous response — skip and keep reading.
		case *pgproto3.CommandComplete:
			commandCompleteSeen = true
		case *pgproto3.ErrorResponse:
			// An ErrorResponse is always followed by ReadyForQuery. We record the
			// error and keep draining until we see ReadyForQuery (after CommandComplete
			// from our query, or directly after the error if the error is from our query).
			// Treat the error as fulfilling the CommandComplete requirement.
			commandCompleteSeen = true
			queryErr = fmt.Errorf("%s failed: %s %s (%s)", query, m.Severity, m.Code, m.Message)
		}
	}
}

// drainToReady drains pending backend messages until ReadyForQuery.
// It should only be called when TxStatus indicates there are pending messages
// (i.e., TxStatus != 'I'). Calling on an idle connection would block.
func (s *SessionClient) drainToReady(ctx context.Context) error {
	for {
		msg, err := s.Receive(ctx)
		if err != nil {
			return fmt.Errorf("drain receive: %w", err)
		}

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			return nil
		case *pgproto3.ErrorResponse:
			// ErrorResponse is followed by ReadyForQuery, keep draining.
			_ = m
		}
	}
}
