package upstream_client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SessionClient struct {
	conn *pgxpool.Conn
}

func Initialize(conn *pgxpool.Conn) *SessionClient {
	return &SessionClient{
		conn: conn,
	}
}

func (s *SessionClient) TxStatus() byte {
	return s.conn.Conn().PgConn().TxStatus()
}

func (s *SessionClient) Send(msgs ...pgproto3.FrontendMessage) error {
	conn := s.conn.Conn().PgConn().Conn()

	for _, msg := range msgs {
		data, err := msg.Encode(nil)
		if err != nil {
			return err
		}
		if _, err := conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func (s *SessionClient) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	return s.conn.Conn().PgConn().ReceiveMessage(ctx)
}

// Release resets the connection state and returns it to the pool.
// It executes DISCARD ALL to ensure a clean state for the next consumer.
//
// NOTE: This is safe because Release() is only called from session.Close()
// AFTER wg.Wait() ensures the upstreamReadLoop goroutine has exited.
func (s *SessionClient) Release() {
	if err := s.reset(); err != nil {
		// If reset fails, destroy the connection instead of returning it to the pool
		s.conn.Hijack().Close(context.Background())
		return
	}
	s.conn.Release()
}

// reset executes DISCARD ALL to clear any session-level state.
func (s *SessionClient) reset() error {
	// Use a fresh context since the session context is already cancelled
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Drain any pending messages so we start from a clean protocol boundary.
	if err := s.drainToReady(ctx); err != nil {
		return err
	}

	// If we're not idle, attempt a rollback before discarding state.
	if s.TxStatus() != 'I' {
		if err := s.simpleQuery(ctx, "ROLLBACK"); err != nil {
			return err
		}
	}

	if err := s.simpleQuery(ctx, "DISCARD ALL"); err != nil {
		return err
	}

	return nil
}

func (s *SessionClient) simpleQuery(ctx context.Context, query string) error {
	if err := s.Send(&pgproto3.Query{String: query}); err != nil {
		return fmt.Errorf("send %s: %w", query, err)
	}

	// Consume response: expect CommandComplete + ReadyForQuery
	for {
		msg, err := s.Receive(ctx)
		if err != nil {
			return fmt.Errorf("receive %s response: %w", query, err)
		}

		switch msg.(type) {
		case *pgproto3.ReadyForQuery:
			return nil
		case *pgproto3.CommandComplete:
			// Expected, continue to ReadyForQuery
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("%s failed", query)
		}
	}
}

// drainToReady drains any pending backend messages until ReadyForQuery or timeout.
// If no message is pending, it returns quickly.
func (s *SessionClient) drainToReady(ctx context.Context) error {
	// Probe quickly for any queued messages to avoid blocking on idle connections.
	probeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	msg, err := s.Receive(probeCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("drain receive: %w", err)
	}

	for {
		switch msg.(type) {
		case *pgproto3.ReadyForQuery:
			return nil
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("drain failed")
		}

		msg, err = s.Receive(ctx)
		if err != nil {
			return fmt.Errorf("drain receive: %w", err)
		}
	}
}
