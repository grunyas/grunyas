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

	// Only drain if the connection is not idle — avoids the speculative probe
	// that can corrupt protocol state on an idle connection.
	if s.TxStatus() != 'I' {
		if err := s.drainToReady(ctx); err != nil {
			return fmt.Errorf("drain pending messages: %w", err)
		}
	}

	// If still not idle after draining, attempt a rollback.
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

	// Consume response: expect CommandComplete + ReadyForQuery
	var queryErr error
	for {
		msg, err := s.Receive(ctx)
		if err != nil {
			return fmt.Errorf("receive %s response: %w", query, err)
		}

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			return queryErr
		case *pgproto3.CommandComplete:
			// Expected, continue to ReadyForQuery
		case *pgproto3.ErrorResponse:
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
