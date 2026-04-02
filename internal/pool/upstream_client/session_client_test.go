package upstream_client

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// mockConn simulates the backend connection for testing the drain/reset algorithms.
type mockConn struct {
	messages []pgproto3.BackendMessage
	txStatus byte
	sent     []string // queries sent via simpleQuery
}

// testableClient mirrors the SessionClient's drain and reset algorithms
// but operates on mockConn instead of a real pgxpool connection.
type testableClient struct {
	conn *mockConn
}

func (t *testableClient) TxStatus() byte {
	return t.conn.txStatus
}

func (t *testableClient) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(t.conn.messages) == 0 {
		// Block until context cancelled (simulates idle connection)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	msg := t.conn.messages[0]
	t.conn.messages = t.conn.messages[1:]
	if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
		t.conn.txStatus = rfq.TxStatus
	}
	return msg, nil
}

func (t *testableClient) drainToReady(ctx context.Context) error {
	for {
		msg, err := t.Receive(ctx)
		if err != nil {
			return fmt.Errorf("drain receive: %w", err)
		}

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			return nil
		case *pgproto3.ErrorResponse:
			_ = m
		}
	}
}

func (t *testableClient) simpleQuery(ctx context.Context, query string) error {
	t.conn.sent = append(t.conn.sent, query)

	var queryErr error
	for {
		msg, err := t.Receive(ctx)
		if err != nil {
			return fmt.Errorf("receive %s response: %w", query, err)
		}

		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			return queryErr
		case *pgproto3.CommandComplete:
			// expected
		case *pgproto3.ErrorResponse:
			queryErr = fmt.Errorf("%s failed: %s %s (%s)", query, m.Severity, m.Code, m.Message)
		}
	}
}

func (t *testableClient) reset(ctx context.Context) error {
	if t.TxStatus() != 'I' {
		if err := t.drainToReady(ctx); err != nil {
			return fmt.Errorf("drain pending messages: %w", err)
		}
	}

	if t.TxStatus() != 'I' {
		if err := t.simpleQuery(ctx, "ROLLBACK"); err != nil {
			return err
		}
	}

	if err := t.simpleQuery(ctx, "DISCARD ALL"); err != nil {
		return err
	}

	return nil
}

func TestDrainToReadyConsumesUntilRFQ(t *testing.T) {
	tc := &testableClient{conn: &mockConn{
		txStatus: 'T',
		messages: []pgproto3.BackendMessage{
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
			&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		},
	}}

	ctx := context.Background()
	if err := tc.drainToReady(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.TxStatus() != 'I' {
		t.Fatalf("expected TxStatus 'I', got '%c'", tc.TxStatus())
	}
}

func TestDrainToReadyHandlesErrorResponse(t *testing.T) {
	tc := &testableClient{conn: &mockConn{
		txStatus: 'E',
		messages: []pgproto3.BackendMessage{
			&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "syntax error"},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		},
	}}

	ctx := context.Background()
	if err := tc.drainToReady(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tc.TxStatus() != 'I' {
		t.Fatalf("expected TxStatus 'I', got '%c'", tc.TxStatus())
	}
}

func TestDrainToReadyTimesOut(t *testing.T) {
	tc := &testableClient{conn: &mockConn{
		txStatus: 'T',
		messages: nil, // no messages = blocks
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := tc.drainToReady(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSimpleQuerySuccess(t *testing.T) {
	tc := &testableClient{conn: &mockConn{
		txStatus: 'I',
		messages: []pgproto3.BackendMessage{
			&pgproto3.CommandComplete{CommandTag: []byte("DISCARD ALL")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		},
	}}

	ctx := context.Background()
	if err := tc.simpleQuery(ctx, "DISCARD ALL"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tc.conn.sent) != 1 || tc.conn.sent[0] != "DISCARD ALL" {
		t.Fatalf("expected sent=[DISCARD ALL], got %v", tc.conn.sent)
	}
}

func TestSimpleQueryHandlesErrorThenRFQ(t *testing.T) {
	tc := &testableClient{conn: &mockConn{
		txStatus: 'I',
		messages: []pgproto3.BackendMessage{
			&pgproto3.ErrorResponse{Severity: "ERROR", Code: "25P01", Message: "no transaction"},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		},
	}}

	ctx := context.Background()
	err := tc.simpleQuery(ctx, "ROLLBACK")
	if err == nil {
		t.Fatal("expected error from ROLLBACK")
	}
	if !strings.Contains(err.Error(), "25P01") {
		t.Fatalf("expected error to contain code 25P01, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no transaction") {
		t.Fatalf("expected error to contain message, got: %v", err)
	}
}

func TestResetFlowIdleConnection(t *testing.T) {
	// Connection already idle — should skip drain and rollback, go straight to DISCARD ALL.
	tc := &testableClient{conn: &mockConn{
		txStatus: 'I',
		messages: []pgproto3.BackendMessage{
			&pgproto3.CommandComplete{CommandTag: []byte("DISCARD ALL")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := tc.reset(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tc.conn.sent) != 1 || tc.conn.sent[0] != "DISCARD ALL" {
		t.Fatalf("expected only DISCARD ALL, got %v", tc.conn.sent)
	}
}

func TestResetFlowInTransaction(t *testing.T) {
	// Connection in transaction — should drain, then rollback, then discard.
	tc := &testableClient{conn: &mockConn{
		txStatus: 'T',
		messages: []pgproto3.BackendMessage{
			// drain messages
			&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
			&pgproto3.ReadyForQuery{TxStatus: 'T'}, // still in transaction after drain
			// rollback response
			&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
			// discard all response
			&pgproto3.CommandComplete{CommandTag: []byte("DISCARD ALL")},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
		},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := tc.reset(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tc.conn.sent) != 2 {
		t.Fatalf("expected 2 queries, got %v", tc.conn.sent)
	}
	if tc.conn.sent[0] != "ROLLBACK" {
		t.Fatalf("expected first query to be ROLLBACK, got %s", tc.conn.sent[0])
	}
	if tc.conn.sent[1] != "DISCARD ALL" {
		t.Fatalf("expected second query to be DISCARD ALL, got %s", tc.conn.sent[1])
	}
}
