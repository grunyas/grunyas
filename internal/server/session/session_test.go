package session

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/server/downstream_client"
	"github.com/grunyas/grunyas/internal/server/types"
	"go.uber.org/zap"
)

type mockProxyServer struct {
	ctx      context.Context
	log      *zap.Logger
	cfg      *config.Config
	upstream *mockUpstream
}

func (m *mockProxyServer) GetContext() context.Context {
	return m.ctx
}

func (m *mockProxyServer) GetLogger() *zap.Logger {
	return m.log
}

func (m *mockProxyServer) GetConfig() *config.Config {
	return m.cfg
}

func (m *mockProxyServer) PoolStats() types.PoolStats {
	return types.PoolStats{}
}

func (m *mockProxyServer) Authenticate(user, password string) (types.UpstreamClientInterface, error) {
	if m.upstream == nil {
		return &mockUpstream{}, nil
	}
	return m.upstream, nil
}

type mockUpstream struct {
	txStatus    byte
	releaseFunc func()
	responses   chan pgproto3.BackendMessage
}

func (m *mockUpstream) SendSimpleQuery(ctx context.Context, query string) (types.ResultReader, error) {
	return &mockResultReader{}, nil
}

func (m *mockUpstream) Send(msgs ...pgproto3.FrontendMessage) error {
	return nil
}

func (m *mockUpstream) TxStatus() byte {
	return m.txStatus
}

func (m *mockUpstream) Release() {
	if m.releaseFunc != nil {
		m.releaseFunc()
	}
}

func (m *mockUpstream) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-m.responses:
		return res, nil
	}
}

func (m *mockUpstream) enqueue(msgs ...pgproto3.BackendMessage) {
	for _, msg := range msgs {
		m.responses <- msg
	}
}

type mockResultReader struct {
	next bool
}

func (m *mockResultReader) NextResult() bool {
	if !m.next {
		m.next = true
		return true
	}
	return false
}

func (m *mockResultReader) FieldDescriptions() []pgproto3.FieldDescription { return nil }

func (m *mockResultReader) NextRow() bool { return false }

func (m *mockResultReader) Values() [][]byte { return nil }

func (m *mockResultReader) Close() (pgproto3.CommandComplete, error) {
	return pgproto3.CommandComplete{CommandTag: []byte("OK")}, nil
}

// startSession spins up a Session with a connected TCP loopback pair and returns
// the session, the client side of the connection, a done channel that closes
// when Run exits, a cleanup function, and the mock upstream.
// Uses TCP loopback instead of net.Pipe to get proper kernel buffering.
func startSession(t *testing.T, parent context.Context) (*Session, net.Conn, <-chan struct{}, func(), *mockUpstream) {
	t.Helper()

	// Create a TCP listener on loopback
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}

	// Accept connection in a goroutine
	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()

	// Connect to the listener
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	// Get the server side of the connection
	serverConn := <-serverConnCh
	_ = listener.Close()

	defaultCfg := config.Default()
	mockUpstream := &mockUpstream{
		txStatus:  'I',
		responses: make(chan pgproto3.BackendMessage, 64),
	}
	mockSrv := &mockProxyServer{
		ctx:      parent,
		log:      zap.NewNop(),
		cfg:      &defaultCfg,
		upstream: mockUpstream,
	}

	down := downstream_client.Initialize(serverConn, nil, false, zap.NewNop())
	sess := Initialize(mockSrv, down)

	done := make(chan struct{})
	go func() {
		sess.Run()
		close(done)
	}()

	cleanup := func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}

	consumeWelcomeFull(t, clientConn)

	// Small delay to ensure session's read loops are fully running
	time.Sleep(10 * time.Millisecond)

	return sess, clientConn, done, cleanup, mockUpstream
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("session did not finish in time")
	}
}

func consumeWelcome(t *testing.T, frontend *pgproto3.Frontend) {
	t.Helper()

	// Session.Run() sends ParameterStatus messages, BackendKeyData, and ReadyForQuery
	// We need to consume these messages
	for {
		msg, err := frontend.Receive()
		if err != nil {
			t.Fatalf("failed to receive welcome message: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			// ReadyForQuery is the last message in the welcome sequence
			break
		}
	}
}

func consumeWelcomeFull(t *testing.T, conn net.Conn) {
	t.Helper()

	frontend := pgproto3.NewFrontend(conn, conn)

	// Session.Run() now starts with Startup flow.
	// 1. Send StartupMessage
	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "postgres"},
	})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush startup: %v", err)
	}

	// 2. Consume AuthenticationCleartextPassword request
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive auth request: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); !ok {
		t.Fatalf("expected AuthenticationCleartextPassword, got %T", msg)
	}

	// 3. Send PasswordMessage
	frontend.Send(&pgproto3.PasswordMessage{Password: "postgres"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush password: %v", err)
	}

	// 4. Consume AuthenticationOk
	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive auth ok: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk, got %T", msg)
	}

	// 5. Consume Welcome messages (ParameterStatus etc)
	consumeWelcome(t, frontend)
}

func TestSessionUpdatesLastActiveOnRead(t *testing.T) {
	parentCtx := t.Context()

	sess, clientConn, done, cleanup, upstream := startSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	before := sess.LastActive()

	// Enqueue a response so the upstream reader can forward it.
	// Minimal response for this test.
	upstream.enqueue(&pgproto3.ReadyForQuery{TxStatus: 'I'})

	frontend.Send(&pgproto3.Query{String: "select 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush failed: %v", err)
	}

	if _, err := frontend.Receive(); err != nil { // ReadyForQuery
		t.Fatalf("failed to read ready for query: %v", err)
	}

	after := sess.LastActive()
	if !after.After(before) {
		t.Fatalf("LastActive not updated; before=%v after=%v", before, after)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionIDsIncrement(t *testing.T) {
	defaultCfg := config.Default()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mockSrv := &mockProxyServer{
		ctx: ctx,
		log: zap.NewNop(),
		cfg: &defaultCfg,
	}

	s1Conn, s1Peer := net.Pipe()
	s1 := Initialize(mockSrv, downstream_client.Initialize(s1Conn, nil, false, zap.NewNop()))
	s2Conn, s2Peer := net.Pipe()
	s2 := Initialize(mockSrv, downstream_client.Initialize(s2Conn, nil, false, zap.NewNop()))

	defer s1.Close()
	defer s2.Close()
	defer func() { _ = s1Peer.Close() }()
	defer func() { _ = s2Peer.Close() }()

	if s2.ID() <= s1.ID() {
		t.Fatalf("expected s2 id > s1 id, got s1=%d s2=%d", s1.ID(), s2.ID())
	}
}

func TestSessionHandlesSimpleQuery(t *testing.T) {
	parentCtx := t.Context()

	_, clientConn, done, cleanup, upstream := startSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Simulate upstream response for a simple query.
	// We need CommandComplete then ReadyForQuery to match expectations.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("OK")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "select 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush query: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive command complete: %v", err)
	}
	cc, ok := msg.(*pgproto3.CommandComplete)
	if !ok {
		t.Fatalf("expected CommandComplete, got %T", msg)
	}
	if string(cc.CommandTag) != "OK" {
		t.Fatalf("unexpected command tag %q", cc.CommandTag)
	}

	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive ReadyForQuery: %v", err)
	}
	if _, ok := msg.(*pgproto3.ReadyForQuery); !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionHandlesExtendedFlow(t *testing.T) {
	parentCtx := t.Context()

	_, clientConn, done, cleanup, upstream := startSession(t, parentCtx)
	defer cleanup()

	// Queue expected responses from upstream
	upstream.enqueue(
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("EXECUTE")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Parse{Name: "stmt1", Query: "select 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush parse: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive parse complete: %v", err)
	}
	if _, ok := msg.(*pgproto3.ParseComplete); !ok {
		t.Fatalf("expected ParseComplete, got %T", msg)
	}

	frontend.Send(&pgproto3.Bind{DestinationPortal: "portal1", PreparedStatement: "stmt1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush bind: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive bind complete: %v", err)
	}
	if _, ok := msg.(*pgproto3.BindComplete); !ok {
		t.Fatalf("expected BindComplete, got %T", msg)
	}

	frontend.Send(&pgproto3.Execute{Portal: "portal1", MaxRows: 0})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush execute: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive command complete after execute: %v", err)
	}
	cc, ok := msg.(*pgproto3.CommandComplete)
	if !ok {
		t.Fatalf("expected CommandComplete, got %T", msg)
	}
	if string(cc.CommandTag) != "EXECUTE" {
		t.Fatalf("unexpected command tag %q", cc.CommandTag)
	}

	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush sync: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive ReadyForQuery after sync: %v", err)
	}
	if _, ok := msg.(*pgproto3.ReadyForQuery); !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionHandlesMultipleExecutesBeforeSync(t *testing.T) {
	parentCtx := t.Context()

	_, clientConn, done, cleanup, upstream := startSession(t, parentCtx)
	defer cleanup()

	// Queue expected responses: ParseComplete, BindComplete, 3x CommandComplete, ReadyForQuery
	upstream.enqueue(
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("EXECUTE")},
		&pgproto3.CommandComplete{CommandTag: []byte("EXECUTE")},
		&pgproto3.CommandComplete{CommandTag: []byte("EXECUTE")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Parse a statement
	frontend.Send(&pgproto3.Parse{Name: "stmt1", Query: "select 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush parse: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive parse complete: %v", err)
	}
	if _, ok := msg.(*pgproto3.ParseComplete); !ok {
		t.Fatalf("expected ParseComplete, got %T", msg)
	}

	// Bind to a portal
	frontend.Send(&pgproto3.Bind{DestinationPortal: "portal1", PreparedStatement: "stmt1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush bind: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive bind complete: %v", err)
	}
	if _, ok := msg.(*pgproto3.BindComplete); !ok {
		t.Fatalf("expected BindComplete, got %T", msg)
	}

	// Execute multiple times before Sync
	for i := 0; i < 3; i++ {
		frontend.Send(&pgproto3.Execute{Portal: "portal1", MaxRows: 0})
		if err := frontend.Flush(); err != nil {
			t.Fatalf("failed to flush execute %d: %v", i, err)
		}

		msg, err = frontend.Receive()
		if err != nil {
			t.Fatalf("failed to receive command complete for execute %d: %v", i, err)
		}
		cc, ok := msg.(*pgproto3.CommandComplete)
		if !ok {
			t.Fatalf("expected CommandComplete for execute %d, got %T", i, msg)
		}
		if string(cc.CommandTag) != "EXECUTE" {
			t.Fatalf("unexpected command tag %q for execute %d", cc.CommandTag, i)
		}

		// Should NOT receive ReadyForQuery here - that only comes after Sync
	}

	// Now send Sync
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush sync: %v", err)
	}

	// NOW we should get ReadyForQuery
	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive ReadyForQuery after sync: %v", err)
	}
	if _, ok := msg.(*pgproto3.ReadyForQuery); !ok {
		t.Fatalf("expected ReadyForQuery after sync, got %T", msg)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionCloseWithErrorSendsMessage(t *testing.T) {
	parentCtx := t.Context()

	sess, clientConn, done, cleanup, _ := startSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Trigger error close
	go func() {
		// Wait a bit to ensure session is running
		time.Sleep(10 * time.Millisecond)
		_ = sess.CloseWithError("FATAL", "57P01", "terminating connection due to idle timeout")
	}()

	// We should receive an ErrorResponse
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive message: %v", err)
	}

	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}

	if errResp.Code != "57P01" {
		t.Fatalf("expected code 57P01, got %s", errResp.Code)
	}
	if errResp.Message != "terminating connection due to idle timeout" {
		t.Fatalf("unexpected message: %s", errResp.Message)
	}
	if errResp.Severity != "FATAL" {
		t.Fatalf("unexpected severity: %s", errResp.Severity)
	}

	// Connection should be closed
	// Try to receive again - should get EOF or similar error
	_, err = frontend.Receive()
	if err == nil {
		t.Fatal("expected connection to be closed, but receive succeeded")
	}

	// waitDone handles the session.Run() returning
	waitDone(t, done)
}

func TestSessionHandlesDescribeStatement(t *testing.T) {
	parentCtx := t.Context()

	_, clientConn, done, cleanup, upstream := startSession(t, parentCtx)
	defer cleanup()

	// Describe Statement expects ParameterDescription AND RowDescription (or NoData)
	// Current implementation fails to consume both.
	upstream.enqueue(
		&pgproto3.ParameterDescription{ParameterOIDs: []uint32{23}},
		&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("col1")}}},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Send Describe
	frontend.Send(&pgproto3.Describe{ObjectType: 'S', Name: "stmt1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush describe: %v", err)
	}

	// Receive ParamDesc
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive param desc: %v", err)
	}
	if _, ok := msg.(*pgproto3.ParameterDescription); !ok {
		t.Fatalf("expected ParameterDescription, got %T", msg)
	}

	// Receive RowDesc
	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive row desc: %v", err)
	}
	if _, ok := msg.(*pgproto3.RowDescription); !ok {
		t.Fatalf("expected RowDescription, got %T", msg)
	}

	// Send Sync to verify strict sync
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush sync: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive ReadyForQuery: %v", err)
	}
	if _, ok := msg.(*pgproto3.ReadyForQuery); !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}
