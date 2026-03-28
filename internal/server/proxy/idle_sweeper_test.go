package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/server/session"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

func TestIdleSweeper(t *testing.T) {
	idleTimeout := 50 * time.Millisecond
	sweeper := newIdleSweeper(idleTimeout)

	ctx := t.Context()
	s1, c1 := newTestSession(ctx)
	s2, c2 := newTestSession(ctx)
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	t0 := time.Now()
	if !sweeper.Track(s1) {
		t.Fatal("expected tracking to succeed for s1")
	}

	time.Sleep(idleTimeout / 2) // ensure s2 is tracked later than s1

	if !sweeper.Track(s2) {
		t.Fatal("expected tracking to succeed for s2")
	}
	t1 := time.Now()

	// Advance logical time to after s1's deadline but before s2's.
	expireNow := t0.Add(idleTimeout + time.Millisecond)
	expired := sweeper._expire(expireNow)
	if len(expired) != 1 || expired[0].ID() != s1.ID() {
		t.Fatalf("expected only s1 to expire, got: %+v", expired)
	}

	// Move further ahead to expire s2.
	expireLater := t1.Add(idleTimeout + time.Millisecond)
	expired = sweeper._expire(expireLater)
	if len(expired) != 1 || expired[0].ID() != s2.ID() {
		t.Fatalf("expected only s2 to expire, got: %+v", expired)
	}
}

func newTestSession(ctx context.Context) (types.Expirable, net.Conn) {
	serverConn, clientConn := net.Pipe()

	// Create a mock server that implements types.ProxyServer
	defaultCfg := config.Default()
	mockSrv := &mockProxyServer{
		ctx: ctx,
		log: zap.NewNop(),
		cfg: &defaultCfg,
	}

	down := &mockDownstream{
		conn:    serverConn,
		backend: pgproto3.NewBackend(serverConn, serverConn),
	}

	sess := session.Initialize(mockSrv, down)
	return sess, clientConn
}

type mockUpstream struct{}

func (m *mockUpstream) SendSimpleQuery(ctx context.Context, query string) (types.ResultReader, error) {
	return &mockResultReader{}, nil
}

func (m *mockUpstream) Send(msgs ...pgproto3.FrontendMessage) error {
	return nil
}

func (m *mockUpstream) TxStatus() byte {
	return 'I'
}

func (m *mockUpstream) Release() error { return nil }

func (m *mockUpstream) Kill() error { return nil }

func (m *mockUpstream) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	return &pgproto3.ReadyForQuery{TxStatus: 'I'}, nil
}

type mockResultReader struct{}

func (m *mockResultReader) NextResult() bool                               { return false }
func (m *mockResultReader) FieldDescriptions() []pgproto3.FieldDescription { return nil }
func (m *mockResultReader) NextRow() bool                                  { return false }
func (m *mockResultReader) Values() [][]byte                               { return nil }
func (m *mockResultReader) Close() (pgproto3.CommandComplete, error) {
	return pgproto3.CommandComplete{CommandTag: []byte("OK")}, nil
}

type mockDownstream struct {
	conn    net.Conn
	backend *pgproto3.Backend
}

func (m *mockDownstream) Startup() (string, string, error) {
	return "", "", nil
}

func (m *mockDownstream) Handshake() error {
	m.backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return m.backend.Flush()
}

func (m *mockDownstream) Receive() (pgproto3.FrontendMessage, error) {
	return m.backend.Receive()
}

func (m *mockDownstream) Send(msgs ...pgproto3.BackendMessage) error {
	for _, msg := range msgs {
		m.backend.Send(msg)
	}
	return m.backend.Flush()
}

func (m *mockDownstream) Close() error {
	return m.conn.Close()
}

type mockProxyServer struct {
	ctx context.Context
	log *zap.Logger
	cfg *config.Config
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

func (m *mockProxyServer) AuthenticateUser(user, password string) error {
	return nil
}

func (m *mockProxyServer) AcquireUpstream() (types.UpstreamClientInterface, error) {
	return &mockUpstream{}, nil
}
