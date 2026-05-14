package session

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/auth"
	"github.com/grunyas/grunyas/internal/classifier"
	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/server/downstream_client"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/xdg-go/scram"
	"go.uber.org/zap"
)

type mockProxyServer struct {
	ctx         context.Context
	log         *zap.Logger
	cfg         *config.Config
	upstream    *mockUpstream
	acquireFunc func() (types.UpstreamClientInterface, error)
	authn       *auth.Authenticator

	// portOverride lets compat-port tests set Port() to "compat".  Empty
	// string means default ("write").
	portOverride string

	// acquireForSQLFunc lets compat-port tests intercept the SQL passed to
	// AcquireUpstreamForSQL so they can assert routing decisions.  When nil,
	// AcquireUpstream() is used.
	acquireForSQLFunc func(sql string) (types.UpstreamClientInterface, error)

	// acquiredSQLs records every SQL string passed to AcquireUpstreamForSQL.
	// Tests use this to verify the buffered BEGIN's SQL was NOT passed (only
	// the next-statement's SQL drives the lease).
	acquiredSQLs []string
	acquiredMu   sync.Mutex

	// leaseRole is returned as the Role in AcquireUpstreamLease.
	// Default "primary"; set to "replica" for reclassification tests.
	leaseRole string

	// acquireLeaseFunc overrides AcquireUpstreamLease when non-nil.
	// When set, leaseRole is ignored.
	acquireLeaseFunc func(sql string, pwwActive bool, pwwRemaining int) (*types.UpstreamLease, error)

	// rejectCompatReclassificationCount records calls to RejectCompatReclassification.
	rejectCompatReclassificationCount atomic.Int64
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

func (m *mockProxyServer) GetAuthMethod() types.AuthMethod {
	if m.authn != nil {
		return m.authn.Method()
	}
	return types.AuthPlain
}

func (m *mockProxyServer) Authenticate(user, password string) error {
	if m.authn != nil {
		return m.authn.Authenticate(user, password)
	}
	return nil
}

func (m *mockProxyServer) AuthenticateMD5(user, clientHash string, salt [4]byte) error {
	if m.authn != nil {
		return m.authn.AuthenticateMD5(user, clientHash, salt)
	}
	return nil
}

func (m *mockProxyServer) NewSCRAMSession() (types.SCRAMSession, error) {
	if m.authn != nil {
		return m.authn.NewSCRAMSession()
	}
	return nil, fmt.Errorf("SCRAM not configured in mock")
}

func (m *mockProxyServer) AcquireUpstream() (types.UpstreamClientInterface, error) {
	if m.acquireFunc != nil {
		return m.acquireFunc()
	}
	if m.upstream == nil {
		return &mockUpstream{}, nil
	}
	return m.upstream, nil
}

func (m *mockProxyServer) AcquireUpstreamForSQL(sql string) (types.UpstreamClientInterface, error) {
	m.acquiredMu.Lock()
	m.acquiredSQLs = append(m.acquiredSQLs, sql)
	m.acquiredMu.Unlock()
	if m.acquireForSQLFunc != nil {
		return m.acquireForSQLFunc(sql)
	}
	return m.AcquireUpstream()
}

func (m *mockProxyServer) AcquireUpstreamLease(sql string, postWriteWindowActive bool, postWriteWindowRemainingMs int) (*types.UpstreamLease, error) {
	if m.acquireLeaseFunc != nil {
		return m.acquireLeaseFunc(sql, postWriteWindowActive, postWriteWindowRemainingMs)
	}
	up, err := m.AcquireUpstreamForSQL(sql)
	if err != nil {
		return nil, err
	}
	role := m.leaseRole
	if role == "" {
		role = "primary"
	}
	return &types.UpstreamLease{Client: up, NodeID: "node-1", Role: role}, nil
}

func (m *mockProxyServer) sqlsAcquired() []string {
	m.acquiredMu.Lock()
	defer m.acquiredMu.Unlock()
	out := make([]string, len(m.acquiredSQLs))
	copy(out, m.acquiredSQLs)
	return out
}

func (m *mockProxyServer) Port() string {
	if m.portOverride != "" {
		return m.portOverride
	}
	return "write"
}

func (m *mockProxyServer) PublishDecisionEvent(event interface{}) {
}

func (m *mockProxyServer) RejectReadPortWrite(sql, poolMode string) decisions.Event {
	return decisions.Event{}
}

func (m *mockProxyServer) RejectCompatReclassification(sql, poolMode string) decisions.Event {
	m.rejectCompatReclassificationCount.Add(1)
	return decisions.Event{}
}

func (m *mockProxyServer) rejectCompatReclassificationCallCount() int {
	return int(m.rejectCompatReclassificationCount.Load())
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

func (m *mockUpstream) Flush() error {
	return nil
}

func (m *mockUpstream) TxStatus() byte {
	return m.txStatus
}

func (m *mockUpstream) Release() error {
	if m.releaseFunc != nil {
		m.releaseFunc()
	}
	return nil
}

func (m *mockUpstream) Kill() error {
	if m.releaseFunc != nil {
		m.releaseFunc()
	}
	return nil
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

	defaultCfg := config.Default()
	return startSessionWithConfig(t, parent, defaultCfg, nil, nil)
}

// startSessionWithConfig spins up a Session with a connected TCP loopback pair and returns
// the session, the client side of the connection, a done channel that closes
// when Run exits, a cleanup function, and the mock upstream.
func startSessionWithConfig(t *testing.T, parent context.Context, cfg config.Config, upstream *mockUpstream, acquireFunc func() (types.UpstreamClientInterface, error)) (*Session, net.Conn, <-chan struct{}, func(), *mockUpstream) {
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

	if upstream == nil {
		upstream = &mockUpstream{}
	}
	if upstream.txStatus == 0 {
		upstream.txStatus = 'I'
	}
	if upstream.responses == nil {
		upstream.responses = make(chan pgproto3.BackendMessage, 64)
	}
	mockSrv := &mockProxyServer{
		ctx:         parent,
		log:         zap.NewNop(),
		cfg:         &cfg,
		upstream:    upstream,
		acquireFunc: acquireFunc,
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

	return sess, clientConn, done, cleanup, upstream
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("session did not finish in time")
	}
}

func waitForCount(t *testing.T, counter *atomic.Int32, want int32) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		if counter.Load() >= want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for count %d (got %d)", want, counter.Load())
		case <-ticker.C:
		}
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

func TestSessionTransactionPoolingAcquireReleasePerQuery(t *testing.T) {
	parentCtx := t.Context()

	cfg := config.Default()
	cfg.ServerConfig.PoolMode = config.PoolModeTransaction
	if cfg.ServerConfig.Ports == nil {
		cfg.ServerConfig.Ports = make(map[string]config.PortConfig)
	}
	cfg.ServerConfig.Ports["write"] = config.PortConfig{
		ListenAddr: "127.0.0.1:5711",
		PoolMode:   "transaction",
		SSLMode:    "never",
	}

	var acquireCount atomic.Int32
	var releaseCount atomic.Int32

	upstream := &mockUpstream{
		txStatus:  'I',
		responses: make(chan pgproto3.BackendMessage, 64),
		releaseFunc: func() {
			releaseCount.Add(1)
		},
	}

	acquireFunc := func() (types.UpstreamClientInterface, error) {
		acquireCount.Add(1)
		return upstream, nil
	}

	_, clientConn, done, cleanup, _ := startSessionWithConfig(t, parentCtx, cfg, upstream, acquireFunc)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("OK1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "select 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush query 1: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := frontend.Receive(); err != nil {
			t.Fatalf("failed to receive query 1 response %d: %v", i, err)
		}
	}

	waitForCount(t, &releaseCount, 1)

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("OK2")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "select 2"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush query 2: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := frontend.Receive(); err != nil {
			t.Fatalf("failed to receive query 2 response %d: %v", i, err)
		}
	}

	waitForCount(t, &releaseCount, 2)

	if acquireCount.Load() != 2 {
		t.Fatalf("expected 2 acquisitions, got %d", acquireCount.Load())
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionTransactionPoolingSwitchesToSessionOnSessionState(t *testing.T) {
	parentCtx := t.Context()

	cfg := config.Default()
	cfg.ServerConfig.PoolMode = config.PoolModeTransaction
	if cfg.ServerConfig.Ports == nil {
		cfg.ServerConfig.Ports = make(map[string]config.PortConfig)
	}
	cfg.ServerConfig.Ports["write"] = config.PortConfig{
		ListenAddr: "127.0.0.1:5711",
		PoolMode:   "transaction",
		SSLMode:    "never",
	}

	var releaseCount atomic.Int32

	upstream := &mockUpstream{
		txStatus:  'I',
		responses: make(chan pgproto3.BackendMessage, 64),
		releaseFunc: func() {
			releaseCount.Add(1)
		},
	}

	sess, clientConn, done, cleanup, _ := startSessionWithConfig(t, parentCtx, cfg, upstream, func() (types.UpstreamClientInterface, error) {
		return upstream, nil
	})
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("SET")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "SET search_path TO public"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush query: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := frontend.Receive(); err != nil {
			t.Fatalf("failed to receive response %d: %v", i, err)
		}
	}

	if sess.poolMode != config.PoolModeSession {
		t.Fatalf("expected session mode after session-state query, got %s", sess.poolMode)
	}

	time.Sleep(50 * time.Millisecond)
	if releaseCount.Load() != 0 {
		t.Fatalf("expected no release after switching to session mode, got %d", releaseCount.Load())
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionTransactionPoolingAcquireFailureSendsFatal(t *testing.T) {
	parentCtx := t.Context()

	cfg := config.Default()
	cfg.ServerConfig.PoolMode = config.PoolModeTransaction
	if cfg.ServerConfig.Ports == nil {
		cfg.ServerConfig.Ports = make(map[string]config.PortConfig)
	}
	cfg.ServerConfig.Ports["write"] = config.PortConfig{
		ListenAddr: "127.0.0.1:5711",
		PoolMode:   "transaction",
		SSLMode:    "never",
	}

	_, clientConn, done, cleanup, _ := startSessionWithConfig(t, parentCtx, cfg, nil, func() (types.UpstreamClientInterface, error) {
		return nil, types.ErrPoolExhausted
	})
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Query{String: "select 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("failed to flush query: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("failed to receive error response: %v", err)
	}

	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if errResp.Severity != "FATAL" {
		t.Fatalf("expected FATAL severity, got %s", errResp.Severity)
	}
	if errResp.Code != "53300" {
		t.Fatalf("expected code 53300, got %s", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "connection pool exhausted") {
		t.Fatalf("unexpected error message: %s", errResp.Message)
	}

	waitDone(t, done)
}

// startSessionWithAuthenticator starts a session using a real auth.Authenticator and returns
// the raw client connection without performing any auth exchange, so the caller can drive it.
func startSessionWithAuthenticator(t *testing.T, parent context.Context, authn *auth.Authenticator) (*Session, net.Conn, <-chan struct{}, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	serverConn := <-serverConnCh
	_ = listener.Close()

	defaultCfg := config.Default()
	mockSrv := &mockProxyServer{
		ctx:   parent,
		log:   zap.NewNop(),
		cfg:   &defaultCfg,
		authn: authn,
		upstream: &mockUpstream{
			txStatus:  'I',
			responses: make(chan pgproto3.BackendMessage, 64),
		},
	}

	down := downstream_client.Initialize(serverConn, nil, false, zap.NewNop())
	sess := Initialize(mockSrv, down)

	done := make(chan struct{})
	go func() {
		sess.Run()
		close(done)
	}()

	return sess, clientConn, done, func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}
}

func doPlainAuthExchange(t *testing.T, conn net.Conn, user, password string) error {
	t.Helper()
	frontend := pgproto3.NewFrontend(conn, conn)

	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": user},
	})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush startup: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("receive auth challenge: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); !ok {
		t.Fatalf("expected AuthenticationCleartextPassword, got %T", msg)
	}

	frontend.Send(&pgproto3.PasswordMessage{Password: password})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush password: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		return err
	}
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		return fmt.Errorf("%s: %s", errResp.Code, errResp.Message)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk or ErrorResponse, got %T", msg)
	}
	consumeWelcome(t, frontend)
	return nil
}

func doMD5AuthExchange(t *testing.T, conn net.Conn, user, password string) error {
	t.Helper()
	frontend := pgproto3.NewFrontend(conn, conn)

	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": user},
	})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush startup: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("receive auth challenge: %v", err)
	}
	challenge, ok := msg.(*pgproto3.AuthenticationMD5Password)
	if !ok {
		t.Fatalf("expected AuthenticationMD5Password, got %T", msg)
	}

	hash := downstream_client.ComputeMD5Password(user, password, challenge.Salt)
	frontend.Send(&pgproto3.PasswordMessage{Password: hash})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush password: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		return err
	}
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		return fmt.Errorf("%s: %s", errResp.Code, errResp.Message)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk or ErrorResponse, got %T", msg)
	}
	consumeWelcome(t, frontend)
	return nil
}

func doSCRAMAuthExchange(t *testing.T, conn net.Conn, user, password string) error {
	t.Helper()
	frontend := pgproto3.NewFrontend(conn, conn)

	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": user},
	})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush startup: %v", err)
	}

	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("receive SASL: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationSASL); !ok {
		t.Fatalf("expected AuthenticationSASL, got %T", msg)
	}

	client, err := scram.SHA256.NewClient(user, password, "")
	if err != nil {
		t.Fatalf("create SCRAM client: %v", err)
	}
	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	if err != nil {
		t.Fatalf("SCRAM step 1: %v", err)
	}

	frontend.Send(&pgproto3.SASLInitialResponse{AuthMechanism: "SCRAM-SHA-256", Data: []byte(clientFirst)})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SASLInitialResponse: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		return err
	}
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		return fmt.Errorf("%s: %s", errResp.Code, errResp.Message)
	}
	cont, ok := msg.(*pgproto3.AuthenticationSASLContinue)
	if !ok {
		t.Fatalf("expected AuthenticationSASLContinue, got %T", msg)
	}

	clientFinal, err := conv.Step(string(cont.Data))
	if err != nil {
		t.Fatalf("SCRAM step 2: %v", err)
	}

	frontend.Send(&pgproto3.SASLResponse{Data: []byte(clientFinal)})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SASLResponse: %v", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		return err
	}
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		return fmt.Errorf("%s: %s", errResp.Code, errResp.Message)
	}
	final, ok := msg.(*pgproto3.AuthenticationSASLFinal)
	if !ok {
		t.Fatalf("expected AuthenticationSASLFinal, got %T", msg)
	}

	if _, err := conv.Step(string(final.Data)); err != nil {
		return fmt.Errorf("server verification: %w", err)
	}

	msg, err = frontend.Receive()
	if err != nil {
		return err
	}
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		return fmt.Errorf("%s: %s", errResp.Code, errResp.Message)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk, got %T", msg)
	}
	consumeWelcome(t, frontend)
	return nil
}

func newTestAuth(t *testing.T, method string) *auth.Authenticator {
	t.Helper()
	authn, err := auth.Initialize(config.AuthConfig{
		Method: method, Username: "postgres", Password: "postgres",
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("init auth: %v", err)
	}
	return authn
}

func TestSessionAuthPlain_Success(t *testing.T) {
	_, clientConn, done, cleanup := startSessionWithAuthenticator(t, t.Context(), newTestAuth(t, "plain"))
	defer cleanup()

	if err := doPlainAuthExchange(t, clientConn, "postgres", "postgres"); err != nil {
		t.Fatalf("plain auth should succeed: %v", err)
	}
	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionAuthPlain_WrongPassword(t *testing.T) {
	_, clientConn, done, cleanup := startSessionWithAuthenticator(t, t.Context(), newTestAuth(t, "plain"))
	defer cleanup()

	err := doPlainAuthExchange(t, clientConn, "postgres", "wrong")
	if err == nil {
		t.Fatal("expected plain auth to fail with wrong password")
	}
	if !strings.Contains(err.Error(), "28P01") {
		t.Fatalf("expected error code 28P01, got: %v", err)
	}
	waitDone(t, done)
}

func TestSessionAuthMD5_Success(t *testing.T) {
	_, clientConn, done, cleanup := startSessionWithAuthenticator(t, t.Context(), newTestAuth(t, "md5"))
	defer cleanup()

	if err := doMD5AuthExchange(t, clientConn, "postgres", "postgres"); err != nil {
		t.Fatalf("MD5 auth should succeed: %v", err)
	}
	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionAuthMD5_WrongPassword(t *testing.T) {
	_, clientConn, done, cleanup := startSessionWithAuthenticator(t, t.Context(), newTestAuth(t, "md5"))
	defer cleanup()

	err := doMD5AuthExchange(t, clientConn, "postgres", "wrong")
	if err == nil {
		t.Fatal("expected MD5 auth to fail with wrong password")
	}
	if !strings.Contains(err.Error(), "28P01") {
		t.Fatalf("expected error code 28P01, got: %v", err)
	}
	waitDone(t, done)
}

func TestSessionAuthSCRAM_Success(t *testing.T) {
	_, clientConn, done, cleanup := startSessionWithAuthenticator(t, t.Context(), newTestAuth(t, "scram-sha-256"))
	defer cleanup()

	if err := doSCRAMAuthExchange(t, clientConn, "postgres", "postgres"); err != nil {
		t.Fatalf("SCRAM auth should succeed: %v", err)
	}
	_ = clientConn.Close()
	waitDone(t, done)
}

func TestSessionAuthSCRAM_WrongPassword(t *testing.T) {
	_, clientConn, done, cleanup := startSessionWithAuthenticator(t, t.Context(), newTestAuth(t, "scram-sha-256"))
	defer cleanup()

	err := doSCRAMAuthExchange(t, clientConn, "postgres", "wrong")
	if err == nil {
		t.Fatal("expected SCRAM auth to fail with wrong password")
	}
	if !strings.Contains(err.Error(), "28P01") {
		t.Fatalf("expected error code 28P01, got: %v", err)
	}
	waitDone(t, done)
}

// ----------------------------------------------------------------------------
// M4: compat-port session tests
// ----------------------------------------------------------------------------

// startCompatSession spins up a Session bound to the compat port.  Returns the
// session, the client side of the connection, a done channel, a cleanup, and
// the mock proxy and mock upstream so tests can drive both sides.
func startCompatSession(t *testing.T, parent context.Context) (*Session, net.Conn, <-chan struct{}, func(), *mockProxyServer, *mockUpstream) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	serverConn := <-serverConnCh
	_ = listener.Close()

	upstream := &mockUpstream{
		txStatus:  'I',
		responses: make(chan pgproto3.BackendMessage, 64),
	}

	cfg := config.Default()
	// Compat port runs in transaction-mode pooling.
	cfg.ServerConfig.PoolMode = config.PoolModeTransaction

	mockSrv := &mockProxyServer{
		ctx:          parent,
		log:          zap.NewNop(),
		cfg:          &cfg,
		upstream:     upstream,
		portOverride: "compat",
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
	time.Sleep(10 * time.Millisecond)

	return sess, clientConn, done, cleanup, mockSrv, upstream
}

// recvWithTimeout reads one message from the client side with a deadline so
// tests fail fast on a deadlock instead of hanging indefinitely.
func recvWithTimeout(t *testing.T, frontend *pgproto3.Frontend, conn net.Conn, timeout time.Duration) (pgproto3.BackendMessage, error) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	return frontend.Receive()
}

// TestCompatSessionBeginSyntheticResponse verifies that a Query{"BEGIN"} on the
// compat port produces an immediate synthetic CommandComplete + RFQ{T} from the
// proxy without contacting any upstream.  This is the fix for the BEGIN-deadlock
// bug — a non-pipelined client must be able to send the next query.
func TestCompatSessionBeginSyntheticResponse(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, _ := startCompatSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Client sends BEGIN.  Proxy must reply with CommandComplete + RFQ{T}
	// without acquiring an upstream.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("expected synthetic CommandComplete within 1s (deadlock?): %v", err)
	}
	cc, ok := msg.(*pgproto3.CommandComplete)
	if !ok {
		t.Fatalf("expected CommandComplete, got %T", msg)
	}
	if string(cc.CommandTag) != "BEGIN" {
		t.Fatalf("expected tag BEGIN, got %q", cc.CommandTag)
	}

	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("expected synthetic RFQ: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'T' {
		t.Fatalf("expected RFQ{T}, got RFQ{%c}", rfq.TxStatus)
	}

	// No SQL should have been passed to AcquireUpstreamForSQL yet.
	if got := mockSrv.sqlsAcquired(); len(got) != 0 {
		t.Fatalf("expected no AcquireUpstreamForSQL calls before next statement, got %v", got)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatSessionBeginSelectFlowSwallowsUpstreamBegin verifies the full
// BEGIN→SELECT flow: synthetic response, lease decided by SELECT, BEGIN
// replayed to upstream, upstream's BEGIN response swallowed, SELECT response
// forwarded to client.
func TestCompatSessionBeginSelectFlowSwallowsUpstreamBegin(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Step 1: BEGIN
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush BEGIN: %v", err)
	}
	// Consume synthetic CC + RFQ
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic RFQ: %v", err)
	}

	// Step 2: enqueue upstream responses for replayed BEGIN + SELECT.
	// Upstream replies to BEGIN with CommandComplete + RFQ{T} (these MUST be
	// swallowed by the proxy).  Then for SELECT it replies with
	// RowDescription + DataRow + CommandComplete + RFQ{T} (these MUST flow
	// through to the client).
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")}, // swallowed
		&pgproto3.ReadyForQuery{TxStatus: 'T'},                 // swallowed
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	// Step 3: send SELECT
	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SELECT: %v", err)
	}

	// Step 4: receive the four SELECT-side messages.  CC{BEGIN} and RFQ{T}
	// from the upstream must NOT appear here.
	expectTypes := []string{"*pgproto3.RowDescription", "*pgproto3.DataRow", "*pgproto3.CommandComplete", "*pgproto3.ReadyForQuery"}
	for i, want := range expectTypes {
		msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
		if err != nil {
			t.Fatalf("recv #%d (%s): %v", i, want, err)
		}
		got := fmt.Sprintf("%T", msg)
		if got != want {
			t.Fatalf("recv #%d: expected %s, got %s (%+v)", i, want, got, msg)
		}
		if cc, ok := msg.(*pgproto3.CommandComplete); ok {
			if string(cc.CommandTag) != "SELECT 1" {
				t.Fatalf("expected SELECT 1 tag (BEGIN's CC was not swallowed), got %q", cc.CommandTag)
			}
		}
	}

	// SELECT was the SQL that drove the lease; BEGIN's SQL must NOT have
	// been passed to AcquireUpstreamForSQL.
	got := mockSrv.sqlsAcquired()
	if len(got) != 1 || got[0] != "SELECT 1" {
		t.Fatalf("expected exactly one acquire with SQL=\"SELECT 1\", got %v", got)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatSessionBeginUpstreamBeginFails verifies that when the upstream
// replies to the replayed BEGIN with a failed RFQ (status != 'T'), the proxy
// surfaces an ErrorResponse + RFQ{I} to the client so it can recover instead
// of hanging in swallow mode forever.
func TestCompatSessionBeginUpstreamBeginFails(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, _, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush BEGIN: %v", err)
	}
	// Consume synthetic CC + RFQ
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic RFQ: %v", err)
	}

	// Enqueue: upstream replies to replayed BEGIN with ErrorResponse + RFQ{I}.
	// The swallow loop should clear on the RFQ (any TxStatus, not only 'T'),
	// surface a 25P02 ErrorResponse + RFQ{I} to the client, then wait for the
	// next client message.
	upstream.enqueue(
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: "BEGIN failed upstream"},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SELECT: %v", err)
	}

	// First message after SELECT: must be the synthesised ErrorResponse from
	// the BEGIN-failed handling, NOT the upstream's ErrorResponse (which is
	// swallowed) and NOT a hang.
	msg, err := recvWithTimeout(t, frontend, clientConn, 2*time.Second)
	if err != nil {
		t.Fatalf("expected ErrorResponse after BEGIN failure (swallow flag stuck?): %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if er.Code != "25P02" {
		t.Fatalf("expected code 25P02 (in failed transaction), got %q", er.Code)
	}
	if !strings.HasPrefix(er.Message, "[grunyas] ") {
		t.Fatalf("expected [grunyas] prefix on synthesized error, got %q", er.Message)
	}

	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("expected RFQ after BEGIN-failed: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I} after BEGIN-failed (client unwound), got RFQ{%c}", rfq.TxStatus)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatSessionParseBeginNotBuffered verifies that an extended-protocol
// Parse{query: "BEGIN"} does NOT trigger the BEGIN-buffering hook.  The hook
// must type-gate on Query, not on the SQL content alone, because Parse expects
// ParseComplete (after Sync) — not CommandComplete + RFQ.
func TestCompatSessionParseBeginNotBuffered(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Enqueue what an upstream would return for Parse + Sync: ParseComplete + RFQ.
	upstream.enqueue(
		&pgproto3.ParseComplete{},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Parse{Name: "p1", Query: "BEGIN"})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush parse: %v", err)
	}

	// First message must be ParseComplete — NOT a synthetic CommandComplete
	// for BEGIN.  If the hook misfired, we would see CC{BEGIN} here.
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv after Parse: %v", err)
	}
	if _, ok := msg.(*pgproto3.ParseComplete); !ok {
		// Specifically guard against the misfire shape:
		if cc, isCC := msg.(*pgproto3.CommandComplete); isCC {
			t.Fatalf("BEGIN-buffering hook misfired on Parse: got CommandComplete{%q} instead of ParseComplete",
				cc.CommandTag)
		}
		t.Fatalf("expected ParseComplete, got %T", msg)
	}

	// Acquire was driven by the Parse's SQL (since the Parse path hits the
	// regular acquire branch).
	if got := mockSrv.sqlsAcquired(); len(got) == 0 {
		t.Fatalf("expected an acquire on Parse, got none")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatSessionAcquireFailsAfterBufferedBegin verifies that when the
// next-statement acquire fails AFTER the BEGIN was buffered (and the synthetic
// RFQ{T} already sent), the session unwinds cleanly with ErrorResponse + RFQ{I}
// rather than terminating with a fatal close.
func TestCompatSessionAcquireFailsAfterBufferedBegin(t *testing.T) {
	parentCtx := t.Context()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverConn := <-serverConnCh
	_ = listener.Close()

	cfg := config.Default()
	cfg.ServerConfig.PoolMode = config.PoolModeTransaction

	// Configure the mock to fail acquire on SELECT.
	mockSrv := &mockProxyServer{
		ctx:          parentCtx,
		log:          zap.NewNop(),
		cfg:          &cfg,
		portOverride: "compat",
		acquireForSQLFunc: func(sql string) (types.UpstreamClientInterface, error) {
			return nil, &types.ProxyError{Code: "57P03", Message: "no primary available"}
		},
	}

	down := downstream_client.Initialize(serverConn, nil, false, zap.NewNop())
	sess := Initialize(mockSrv, down)

	done := make(chan struct{})
	go func() {
		sess.Run()
		close(done)
	}()
	defer func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}()

	consumeWelcomeFull(t, clientConn)
	time.Sleep(10 * time.Millisecond)

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN — buffered, synthetic CC + RFQ{T}
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush BEGIN: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic RFQ: %v", err)
	}

	// SELECT — acquire fails.  Session must surface ErrorResponse + RFQ{I}
	// (NOT a fatal close) so the client can ROLLBACK its synthetic transaction.
	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SELECT: %v", err)
	}

	msg, err := recvWithTimeout(t, frontend, clientConn, 2*time.Second)
	if err != nil {
		t.Fatalf("recv after acquire-fail: %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if er.Code != "57P03" {
		t.Fatalf("expected 57P03 (from underlying acquire failure), got %q", er.Code)
	}
	if !strings.HasPrefix(er.Message, "[grunyas] ") {
		t.Fatalf("expected [grunyas] prefix, got %q", er.Message)
	}

	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv RFQ after acquire-fail: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I} after unwind, got RFQ{%c}", rfq.TxStatus)
	}

	// Session must still be alive — close it cleanly to make sure Run exits.
	_ = clientConn.Close()
	waitDone(t, done)
	_ = sess
}

// ----------------------------------------------------------------------------
// M4 §3 + §4: reclassification rejection and post-write window tests
// ----------------------------------------------------------------------------

// TestCompatReclassificationQueryInsertRejected verifies that a write-classified
// Query on a replica-leased compat transaction is rejected with 25006.
func TestCompatReclassificationQueryInsertRejected(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN → synthetic CC + RFQ{T}
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush BEGIN: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic RFQ: %v", err)
	}

	// Enqueue upstream responses for replayed BEGIN + SELECT.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	// SELECT 1 — leases replica.
	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SELECT: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT response #%d: %v", i, err)
		}
	}

	// INSERT INTO — rejected with 25006.
	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush INSERT: %v", err)
	}

	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv INSERT rejection: %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if er.Code != "25006" {
		t.Fatalf("expected code 25006, got %q", er.Code)
	}
	if !strings.HasPrefix(er.Message, "[grunyas] ") {
		t.Fatalf("expected [grunyas] prefix, got %q", er.Message)
	}

	// Followed by RFQ{E} (Query-style rejection).
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv RFQ after rejection: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("expected RFQ{E} after rejection, got RFQ{%c}", rfq.TxStatus)
	}

	// RejectCompatReclassification was called.
	if n := mockSrv.rejectCompatReclassificationCount.Load(); n != 1 {
		t.Fatalf("expected 1 RejectCompatReclassification call, got %d", n)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationParseInsertRejected verifies that a write-classified
// Parse on a replica-leased compat transaction is rejected WITHOUT a premature RFQ.
func TestCompatReclassificationParseInsertRejected(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush BEGIN: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv synthetic RFQ: %v", err)
	}

	// Enqueue upstream responses for replayed BEGIN + SELECT (extended protocol).
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	// Parse + Bind + Execute + Sync for SELECT → leases replica.
	frontend.Send(&pgproto3.Parse{Name: "sel", Query: "SELECT 1"})
	frontend.Send(&pgproto3.Bind{PreparedStatement: "sel"})
	frontend.Send(&pgproto3.Execute{Portal: ""})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SELECT pipeline: %v", err)
	}
	// Expect ParseComplete, BindComplete, CommandComplete, RFQ{T}
	var msg pgproto3.BackendMessage
	var err error
	var rfq *pgproto3.ReadyForQuery
	for i := 0; i < 4; i++ {
		if _, err = recvWithTimeout(t, frontend, clientConn, 2*time.Second); err != nil {
			t.Fatalf("recv SELECT response #%d: %v", i, err)
		}
	}

	// Parse "INSERT" — rejected with 25006.
	frontend.Send(&pgproto3.Parse{Name: "ins", Query: "INSERT INTO foo VALUES (1)"})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush INSERT pipeline: %v", err)
	}

	// First response: ErrorResponse (no RFQ — Parse-style).
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv Parse rejection: %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if er.Code != "25006" {
		t.Fatalf("expected code 25006, got %q", er.Code)
	}

	// Second response: RFQ{E} for the Sync (via failed-tx branch).
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv RFQ after Parse rejection: %v", err)
	}
	rfq, ok = msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("expected RFQ{E}, got RFQ{%c}", rfq.TxStatus)
	}

	if n := mockSrv.rejectCompatReclassificationCount.Load(); n != 1 {
		t.Fatalf("expected 1 RejectCompatReclassification call, got %d", n)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationAfterRejectionSelectGets25P02 verifies that after a
// reclassification rejection, a subsequent Query gets 25P02 + RFQ{E}.
func TestCompatReclassificationAfterRejectionSelectGets25P02(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN + SELECT → replica lease.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT #%d: %v", i, err)
		}
	}

	// INSERT → reclassification rejection (25006 + RFQ{E}).
	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv 25006: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv first RFQ{E}: %v", err)
	}

	// SELECT 2 in failed-tx → 25P02 + RFQ{E} (catches §3.i).
	frontend.Send(&pgproto3.Query{String: "SELECT 2"})
	frontend.Flush()

	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv 25P02: %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if er.Code != "25P02" {
		t.Fatalf("expected code 25P02, got %q", er.Code)
	}

	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv second RFQ{E}: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("expected RFQ{E}, got RFQ{%c}", rfq.TxStatus)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationBindExecuteSyncConsumed verifies that after a
// reclassification rejection, Bind + Execute are silently consumed and Sync
// produces RFQ{E} only (no duplicate ErrorResponse).  Catches §3.f.
func TestCompatReclassificationBindExecuteSyncConsumed(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN + SELECT → replica lease.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	// Parse "SELECT 1" → leases replica
	frontend.Send(&pgproto3.Parse{Name: "sel", Query: "SELECT 1"})
	frontend.Send(&pgproto3.Bind{PreparedStatement: "sel"})
	frontend.Send(&pgproto3.Execute{Portal: ""})
	frontend.Send(&pgproto3.Sync{})
	frontend.Flush()
	var msg pgproto3.BackendMessage
	var err error
	for i := 0; i < 4; i++ {
		if _, err = recvWithTimeout(t, frontend, clientConn, 2*time.Second); err != nil {
			t.Fatalf("recv SELECT response #%d: %v", i, err)
		}
	}

	// Parse "INSERT" → reclassification rejection (25006, no RFQ).
	frontend.Send(&pgproto3.Parse{Name: "ins", Query: "INSERT INTO foo VALUES (1)"})
	frontend.Send(&pgproto3.Sync{}) // Sync triggers the RFQ
	frontend.Flush()

	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv Parse rejection: %v", err)
	}
	if _, ok := msg.(*pgproto3.ErrorResponse); !ok {
		t.Fatalf("expected ErrorResponse for Parse, got %T", msg)
	}

	// Sync should produce RFQ{E} only — no additional ErrorResponse.
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv Sync RFQ: %v", err)
	}
	if rfq, ok := msg.(*pgproto3.ReadyForQuery); !ok || rfq.TxStatus != 'E' {
		t.Fatalf("expected RFQ{E} for Sync, got %T", msg)
	}

	if n := mockSrv.rejectCompatReclassificationCount.Load(); n != 1 {
		t.Fatalf("expected 1 RejectCompatReclassification call, got %d", n)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationRollbackResets verifies that after a reclassification
// rejection, ROLLBACK is forwarded to the upstream and the compat state resets.
func TestCompatReclassificationRollbackResets(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN + SELECT → replica lease.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT #%d: %v", i, err)
		}
	}

	// INSERT → reclassification rejection.
	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv 25006: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ{E}: %v", err)
	}

	// ROLLBACK → forwarded to upstream.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "ROLLBACK"})
	frontend.Flush()

	// Expect CommandComplete + RFQ{I} from upstream.
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv ROLLBACK CC: %v", err)
	}
	if _, ok := msg.(*pgproto3.CommandComplete); !ok || string(msg.(*pgproto3.CommandComplete).CommandTag) != "ROLLBACK" {
		t.Fatalf("expected ROLLBACK CommandComplete, got %T", msg)
	}
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv ROLLBACK RFQ: %v", err)
	}
	if rfq, ok := msg.(*pgproto3.ReadyForQuery); !ok || rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I} after ROLLBACK, got %T", msg)
	}

	if n := mockSrv.rejectCompatReclassificationCount.Load(); n != 1 {
		t.Fatalf("expected 1 RejectCompatReclassification call, got %d", n)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationCommitNotRejected verifies that COMMIT on a
// replica-leased transaction is NOT rejected (confirms §3.d fix).
func TestCompatReclassificationCommitNotRejected(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN + SELECT → replica lease.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT #%d: %v", i, err)
		}
	}

	// COMMIT — must NOT be rejected.
	frontend.Send(&pgproto3.Query{String: "COMMIT"})
	frontend.Flush()

	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv COMMIT CC: %v", err)
	}
	if _, ok := msg.(*pgproto3.CommandComplete); !ok || string(msg.(*pgproto3.CommandComplete).CommandTag) != "COMMIT" {
		t.Fatalf("expected COMMIT CommandComplete, got %T", msg)
	}
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv COMMIT RFQ: %v", err)
	}
	if rfq, ok := msg.(*pgproto3.ReadyForQuery); !ok || rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I} after COMMIT, got %T", msg)
	}

	if n := mockSrv.rejectCompatReclassificationCount.Load(); n != 0 {
		t.Fatalf("expected 0 RejectCompatReclassification calls (COMMIT not rejected), got %d", n)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationRollbackNotRejected verifies that ROLLBACK on a
// replica-leased transaction is NOT rejected.
func TestCompatReclassificationRollbackNotRejected(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "replica"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT #%d: %v", i, err)
		}
	}

	frontend.Send(&pgproto3.Query{String: "ROLLBACK"})
	frontend.Flush()

	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv ROLLBACK CC: %v", err)
	}
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv ROLLBACK RFQ: %v", err)
	}
	if rfq, ok := msg.(*pgproto3.ReadyForQuery); !ok || rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I}, got %T", msg)
	}

	if n := mockSrv.rejectCompatReclassificationCount.Load(); n != 0 {
		t.Fatalf("expected 0 RejectCompatReclassification calls, got %d", n)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatReclassificationBindAsFirstMessageNoPanic verifies that sending a
// Bind without a prior Parse does not cause a nil-upstream panic (§3.g).
func TestCompatReclassificationBindAsFirstMessageNoPanic(t *testing.T) {
	parentCtx := t.Context()
	_, clientConn, done, cleanup, _, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Enqueue upstream response for the empty-SQL acquire (leases primary).
	upstream.enqueue(
		&pgproto3.ErrorResponse{
			Severity: "ERROR",
			Code:     "07006",
			Message:  "Bind message without prior Parse",
		},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	// Bind without Parse — must not panic.  Upstream should reject.
	frontend.Send(&pgproto3.Bind{PreparedStatement: "nonexistent"})
	frontend.Send(&pgproto3.Execute{Portal: ""})
	frontend.Send(&pgproto3.Sync{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Should get error responses from upstream (not a panic).
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv after Bind: %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse from upstream, got %T (nil-upstream panic?)", msg)
	}
	if er.Code != "07006" {
		t.Fatalf("expected upstream error code 07006, got %q", er.Code)
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// ----------------------------------------------------------------------------
// M4 §4: post-write window lifecycle tests
// ----------------------------------------------------------------------------

// TestCompatPostWriteWindowExplicitCommitOpensWindow verifies that an explicit
// BEGIN; UPDATE; COMMIT; sequence opens the post-write window.
func TestCompatPostWriteWindowExplicitCommitOpensWindow(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}

	// Enqueue UPDATE responses only (BEGIN replay + CC{UPDATE 1} + RFQ{T}).
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "UPDATE foo SET x = 1"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv UPDATE #%d: %v", i, err)
		}
	}

	// Enqueue COMMIT responses only after UPDATE's responses drained.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "COMMIT"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv COMMIT CC: %v", err)
	}
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv COMMIT RFQ: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I}, got RFQ{%c}", rfq.TxStatus)
	}

	if !sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected post-write window to be active after write COMMIT")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowImplicitCommitOpensWindow verifies that an implicit
// INSERT (single Query, no BEGIN) opens the post-write window.
func TestCompatPostWriteWindowImplicitCommitOpensWindow(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	frontend.Flush()

	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv INSERT CC: %v", err)
	}
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv INSERT RFQ: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I}, got RFQ{%c}", rfq.TxStatus)
	}

	if !sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected post-write window to be active after implicit write COMMIT")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowExtendedProtocolOpensWindow verifies that an
// extended-protocol write (Parse+Bind+Execute+Sync) opens the post-write window.
func TestCompatPostWriteWindowExtendedProtocolOpensWindow(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	upstream.enqueue(
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Parse{Name: "ins", Query: "INSERT INTO foo VALUES (1)"})
	frontend.Send(&pgproto3.Bind{PreparedStatement: "ins"})
	frontend.Send(&pgproto3.Execute{Portal: ""})
	frontend.Send(&pgproto3.Sync{})
	frontend.Flush()

	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 2*time.Second); err != nil {
			t.Fatalf("recv response #%d: %v", i, err)
		}
	}

	if !sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected post-write window to be active after extended write COMMIT")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowRollbackDoesNotOpen verifies that ROLLBACK after
// a write does NOT open the post-write window.
func TestCompatPostWriteWindowRollbackDoesNotOpen(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}

	// Enqueue UPDATE responses (BEGIN replay + CC{UPDATE 1} + RFQ{T}).
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "UPDATE foo SET x = 1"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv UPDATE #%d: %v", i, err)
		}
	}

	// Enqueue ROLLBACK responses only after UPDATE's responses drained.
	// Wait for the session to process the ROLLBACK Query first so
	// awaitingRollbackResult is set before the upstream responses arrive.
	frontend.Send(&pgproto3.Query{String: "ROLLBACK"})
	frontend.Flush()

	for i := 0; i < 100; i++ {
		if sess.awaitingRollbackResult {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !sess.awaitingRollbackResult {
		t.Fatal("ROLLBACK not processed by session before enqueue")
	}

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv ROLLBACK CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv ROLLBACK RFQ: %v", err)
	}

	if sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected post-write window NOT to be active after ROLLBACK")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowReadOnlyCommitDoesNotOpen verifies that COMMIT on
// a read-only transaction does NOT open the post-write window.
func TestCompatPostWriteWindowReadOnlyCommitDoesNotOpen(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}

	// Enqueue SELECT responses only.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT #%d: %v", i, err)
		}
	}

	// Enqueue COMMIT responses only after SELECT's responses drained.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv COMMIT CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv COMMIT RFQ: %v", err)
	}

	if sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected post-write window NOT to be active after read-only COMMIT")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowSliding verifies that a second write commit within
// the window resets the window timer (sliding behavior).
func TestCompatPostWriteWindowSliding(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// First implicit INSERT.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv first INSERT #%d: %v", i, err)
		}
	}

	firstOpenedAt := sess.postWriteWindow.OpenedAt
	if firstOpenedAt.IsZero() {
		t.Fatal("expected window to open after first INSERT")
	}

	time.Sleep(5 * time.Millisecond)

	// Second implicit INSERT — should slide the window.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (2)"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv second INSERT #%d: %v", i, err)
		}
	}

	secondOpenedAt := sess.postWriteWindow.OpenedAt
	if secondOpenedAt.Equal(firstOpenedAt) {
		t.Fatal("expected window OpenedAt to slide forward on second INSERT COMMIT")
	}
	if !secondOpenedAt.After(firstOpenedAt) {
		t.Fatal("expected second OpenedAt to be after first")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowDurationMsZero verifies that post_write_window_ms=0
// disables the window entirely.
func TestCompatPostWriteWindowDurationMsZero(t *testing.T) {
	parentCtx := t.Context()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	serverConn := <-serverConnCh
	_ = listener.Close()

	upstream := &mockUpstream{
		txStatus:  'I',
		responses: make(chan pgproto3.BackendMessage, 64),
	}

	cfg := config.Default()
	cfg.ServerConfig.PoolMode = config.PoolModeTransaction
	cfg.ServerConfig.Ports["compat"] = config.PortConfig{
		PostWriteWindowMs: 0,
		PoolMode:          "transaction",
	}

	mockSrv := &mockProxyServer{
		ctx:          parentCtx,
		log:          zap.NewNop(),
		cfg:          &cfg,
		upstream:     upstream,
		portOverride: "compat",
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
	defer cleanup()

	consumeWelcomeFull(t, clientConn)
	time.Sleep(10 * time.Millisecond)

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv INSERT #%d: %v", i, err)
		}
	}

	if sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected window NOT active when DurationMs=0")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowActiveStatePassed verifies that the session passes
// PostWriteWindowActive=true and RemainingMs to AcquireUpstreamLease after
// a write COMMIT, and false after the window expires.
func TestCompatPostWriteWindowActiveStatePassed(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	pwwActiveSeen := false
	pwwRemainingSeen := 0
	mockSrv.acquireLeaseFunc = func(sql string, pwwActive bool, pwwRemaining int) (*types.UpstreamLease, error) {
		pwwActiveSeen = pwwActive
		pwwRemainingSeen = pwwRemaining
		up, err := mockSrv.AcquireUpstream()
		if err != nil {
			return nil, err
		}
		return &types.UpstreamLease{Client: up, NodeID: "node-1", Role: "primary"}, nil
	}

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// First: write COMMIT to open the window.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)
	frontend.Send(&pgproto3.Query{String: "INSERT INTO foo VALUES (1)"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv INSERT #%d: %v", i, err)
		}
	}

	// Second: a SELECT should see the window active.
	upstream.enqueue(
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT #%d: %v", i, err)
		}
	}

	if !pwwActiveSeen {
		t.Fatal("expected PostWriteWindowActive=true for SELECT after write COMMIT")
	}
	if pwwRemainingSeen <= 0 {
		t.Fatalf("expected PostWriteWindowRemainingMs > 0 for SELECT after write COMMIT, got %d", pwwRemainingSeen)
	}

	time.Sleep(time.Duration(sess.postWriteWindow.DurationMs+10) * time.Millisecond)

	// Third: another SELECT should NOT see the window active.
	pwwActiveSeen = false
	pwwRemainingSeen = 0

	upstream.enqueue(
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("2")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 2"})
	frontend.Flush()
	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT2 #%d: %v", i, err)
		}
	}

	if pwwActiveSeen {
		t.Fatal("expected PostWriteWindowActive=false after window expiry")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatPostWriteWindowUpstreamErrorThenRollbackDoesNotOpen verifies that
// an upstream error (RFQ{E}) followed by ROLLBACK does NOT open the window.
func TestCompatPostWriteWindowUpstreamErrorThenRollbackDoesNotOpen(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}

	// Enqueue: BEGIN replay (swallowed), then UPDATE fails with ErrorResponse + RFQ{E}.
	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint"},
		&pgproto3.ReadyForQuery{TxStatus: 'E'},
	)

	frontend.Send(&pgproto3.Query{String: "UPDATE foo SET x = 1 WHERE id = 1"})
	frontend.Flush()

	// Expect: ErrorResponse + RFQ{E} (BEGIN responses are swallowed).
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv ErrorResponse: %v", err)
	}
	if _, ok := msg.(*pgproto3.ErrorResponse); !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("expected RFQ{E}, got RFQ{%c}", rfq.TxStatus)
	}

	// Session state after upstream error.
	if sess.compatTxState != classifier.TxInFailedTransaction {
		t.Fatalf("expected TxInFailedTransaction after upstream error, got %v", sess.compatTxState)
	}
	if sess.compatTransactionFirstClass != classifier.TypeUnknown {
		t.Fatalf("expected TypeUnknown firstClass after error, got %v", sess.compatTransactionFirstClass)
	}

	// Enqueue ROLLBACK responses only after the error is confirmed.
	frontend.Send(&pgproto3.Query{String: "ROLLBACK"})
	frontend.Flush()
	for i := 0; i < 100; i++ {
		if sess.awaitingRollbackResult {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !sess.awaitingRollbackResult {
		t.Fatal("ROLLBACK not processed before enqueue")
	}

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv ROLLBACK CC: %v", err)
	}
	msg, err = recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv ROLLBACK RFQ: %v", err)
	}
	rfq, ok = msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != 'I' {
		t.Fatalf("expected RFQ{I}, got RFQ{%c}", rfq.TxStatus)
	}

	if sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected window NOT active after upstream error + ROLLBACK")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatParseAfterWriteInTxInheritsWrite verifies that Parse-time
// classification updates compatTxState for transaction-state inheritance
// on an existing upstream (§3.k).
func TestCompatParseAfterWriteInTxInheritsWrite(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN + SELECT → acquire primary, firstClass=Read.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.RowDescription{},
		&pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
		&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "SELECT 1"})
	frontend.Flush()
	for i := 0; i < 3; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv SELECT resp #%d: %v", i, err)
		}
	}
	msg, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second)
	if err != nil {
		t.Fatalf("recv SELECT RFQ: %v", err)
	}
	if rfq, ok := msg.(*pgproto3.ReadyForQuery); !ok || rfq.TxStatus != 'T' {
		t.Fatalf("expected RFQ{T}, got %T", msg)
	}

	if sess.compatTxState != classifier.TxInTransactionRead {
		t.Fatalf("expected TxInTransactionRead after SELECT, got %v", sess.compatTxState)
	}

	// Parse{UPDATE} with existing upstream → must set TxInTransactionWrite.
	frontend.Send(&pgproto3.Parse{Name: "u", Query: "UPDATE foo SET x = 1"})
	frontend.Send(&pgproto3.Bind{PreparedStatement: "u"})
	frontend.Send(&pgproto3.Execute{Portal: ""})
	frontend.Send(&pgproto3.Sync{})
	frontend.Flush()

	// Wait for the main loop to process Parse and update compatTxState.
	for i := 0; i < 100; i++ {
		if sess.compatTxState == classifier.TxInTransactionWrite {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if sess.compatTxState != classifier.TxInTransactionWrite {
		t.Fatalf("expected TxInTransactionWrite after Parse{UPDATE}, got %v", sess.compatTxState)
	}

	// Now enqueue responses (after Sync was processed).
	upstream.enqueue(
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv UPDATE resp #%d: %v", i, err)
		}
	}

	_ = clientConn.Close()
	waitDone(t, done)
}

// TestCompatParseRollbackExistingUpstreamDoesNotOpenWindow verifies that
// Parse-driven ROLLBACK on an existing upstream sets awaitingRollbackResult
// and does NOT open the window (§3.l).
func TestCompatParseRollbackExistingUpstreamDoesNotOpenWindow(t *testing.T) {
	parentCtx := t.Context()
	sess, clientConn, done, cleanup, mockSrv, upstream := startCompatSession(t, parentCtx)
	defer cleanup()

	mockSrv.leaseRole = "primary"
	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// BEGIN + UPDATE → acquire primary, firstClass=Write.
	frontend.Send(&pgproto3.Query{String: "BEGIN"})
	frontend.Flush()
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv CC: %v", err)
	}
	if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
		t.Fatalf("recv RFQ: %v", err)
	}

	upstream.enqueue(
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	)

	frontend.Send(&pgproto3.Query{String: "UPDATE foo SET x = 1"})
	frontend.Flush()
	for i := 0; i < 2; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv UPDATE #%d: %v", i, err)
		}
	}

	if sess.compatTxState != classifier.TxInTransactionWrite {
		t.Fatalf("expected TxInTransactionWrite after UPDATE, got %v", sess.compatTxState)
	}

	// Parse{ROLLBACK} + Bind + Execute + Sync on existing upstream.
	frontend.Send(&pgproto3.Parse{Name: "r", Query: "ROLLBACK"})
	frontend.Send(&pgproto3.Bind{PreparedStatement: "r"})
	frontend.Send(&pgproto3.Execute{Portal: ""})
	frontend.Send(&pgproto3.Sync{})
	frontend.Flush()

	// Wait for session to process the Parse{ROLLBACK} and set awaitingRollbackResult.
	for i := 0; i < 100; i++ {
		if sess.awaitingRollbackResult {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !sess.awaitingRollbackResult {
		t.Fatal("expected awaitingRollbackResult after Parse{ROLLBACK}")
	}

	// Enqueue responses only after the Parse was processed.
	upstream.enqueue(
		&pgproto3.ParseComplete{},
		&pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")},
		&pgproto3.ReadyForQuery{TxStatus: 'I'},
	)

	for i := 0; i < 4; i++ {
		if _, err := recvWithTimeout(t, frontend, clientConn, 1*time.Second); err != nil {
			t.Fatalf("recv resp #%d: %v", i, err)
		}
	}

	if sess.postWriteWindow.Active(time.Now()) {
		t.Fatal("expected window NOT active after Parse{ROLLBACK}")
	}

	_ = clientConn.Close()
	waitDone(t, done)
}
