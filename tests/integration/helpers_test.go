//go:build integration
// +build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/server/proxy"
	"go.uber.org/zap"
)

// Integration test helpers for exercising the proxy at the pgproto3 wire level.
type testEnv struct {
	host     string
	port     int
	user     string
	password string
	database string
}

func loadTestEnv(t *testing.T) testEnv {
	t.Helper()

	host := getenvDefault("PGHOST", "127.0.0.1")
	port := getenvDefaultInt(t, "PGPORT", 5432)
	user := getenvDefault("PGUSER", "postgres")
	password := getenvDefault("PGPASSWORD", "postgres")
	database := getenvDefault("PGDATABASE", "postgres")

	return testEnv{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		database: database,
	}
}

func getenvDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvDefaultInt(t *testing.T, key string, fallback int) int {
	t.Helper()

	if value := os.Getenv(key); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("invalid %s=%q: %v", key, value, err)
		}
		return parsed
	}

	return fallback
}

func waitForPostgres(t *testing.T, env testEnv) {
	t.Helper()

	addr := net.JoinHostPort(env.host, strconv.Itoa(env.port))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("postgres not reachable at %s", addr)
}

func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate port: %v", err)
	}
	defer ln.Close()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("failed to parse listener address: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("failed to parse port: %v", err)
	}

	return port
}

func startProxy(t *testing.T, env testEnv) (addr string, stop func()) {
	t.Helper()

	waitForPostgres(t, env)

	cfg := config.Default()
	cfg.ServerConfig.ListenAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg.ServerConfig.AdminAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg.ServerConfig.SSLMode = "never"
	cfg.ServerConfig.MaxSessions = 10
	cfg.ServerConfig.ClientIdleTimeout = 30

	cfg.Auth.Method = "plain"
	cfg.Auth.Username = env.user
	cfg.Auth.Password = env.password

	cfg.BackendConfig.DatabaseHost = env.host
	cfg.BackendConfig.DatabasePort = env.port
	cfg.BackendConfig.DatabaseUser = env.user
	cfg.BackendConfig.DatabasePassword = env.password
	cfg.BackendConfig.DatabaseName = env.database
	cfg.BackendConfig.PoolMinConns = 1
	cfg.BackendConfig.PoolMaxConns = 4
	cfg.BackendConfig.DatabaseConnectTimeoutSeconds = 5

	ctx, cancel := context.WithCancel(context.Background())
	prx := proxy.Initialize(ctx, &cfg, zap.NewNop())

	errCh := make(chan error, 1)
	go func() {
		errCh <- prx.Run()
	}()

	select {
	case <-prx.Ready():
	case err := <-errCh:
		cancel()
		t.Fatalf("proxy failed to start: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("timed out waiting for proxy to be ready")
	}

	stop = func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("proxy returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for proxy shutdown")
		}
	}

	return cfg.ServerConfig.ListenAddr, stop
}

type testClient struct {
	conn net.Conn
	fe   *pgproto3.Frontend
}

func newTestClient(t *testing.T, addr string, env testEnv) *testClient {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}

	fe := pgproto3.NewFrontend(conn, conn)
	startup := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{
			"user":     env.user,
			"database": env.database,
		},
	}
	fe.Send(startup)
	if err := fe.Flush(); err != nil {
		_ = conn.Close()
		t.Fatalf("failed to send startup: %v", err)
	}

	msg, err := fe.Receive()
	if err != nil {
		_ = conn.Close()
		t.Fatalf("failed to receive auth request: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationCleartextPassword); !ok {
		_ = conn.Close()
		t.Fatalf("expected AuthenticationCleartextPassword, got %T", msg)
	}

	fe.Send(&pgproto3.PasswordMessage{Password: env.password})
	if err := fe.Flush(); err != nil {
		_ = conn.Close()
		t.Fatalf("failed to send password: %v", err)
	}

	msg, err = fe.Receive()
	if err != nil {
		_ = conn.Close()
		t.Fatalf("failed to receive AuthenticationOk: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		_ = conn.Close()
		t.Fatalf("expected AuthenticationOk, got %T", msg)
	}

	for {
		msg, err = fe.Receive()
		if err != nil {
			_ = conn.Close()
			t.Fatalf("failed during startup handshake: %v", err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}

	return &testClient{conn: conn, fe: fe}
}

func (c *testClient) close() {
	_ = c.conn.Close()
}

type stepDirection int

const (
	stepClient stepDirection = iota
	stepServer
	stepAssert
)

type step struct {
	dir    stepDirection
	send   pgproto3.FrontendMessage
	expect func(*testing.T, pgproto3.BackendMessage)
	assert func(*testing.T, *testClient)
}

// cStep/sStep/assertStep model the client/server message flow explicitly so we can
// validate protocol ordering without hiding messages in higher-level helpers.
func cStep(msg pgproto3.FrontendMessage) step {
	return step{dir: stepClient, send: msg}
}

func sStep(expect func(*testing.T, pgproto3.BackendMessage)) step {
	return step{dir: stepServer, expect: expect}
}

func assertStep(assert func(*testing.T, *testClient)) step {
	return step{dir: stepAssert, assert: assert}
}

func runSteps(t *testing.T, client *testClient, steps []step) {
	t.Helper()

	for i, st := range steps {
		switch st.dir {
		case stepClient:
			client.fe.Send(st.send)
			if err := client.fe.Flush(); err != nil {
				t.Fatalf("step %d: failed to flush: %v", i+1, err)
			}
		case stepServer:
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("step %d: failed to receive: %v", i+1, err)
			}
			st.expect(t, msg)
		case stepAssert:
			st.assert(t, client)
		default:
			t.Fatalf("step %d: unknown step direction", i+1)
		}
	}
}

// The expect* helpers are intentionally strict to surface protocol ordering bugs.
func expectParseComplete(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		t.Fatalf("expected ParseComplete, got ErrorResponse code=%s severity=%s message=%s", errResp.Code, errResp.Severity, errResp.Message)
	}
	if _, ok := msg.(*pgproto3.ParseComplete); !ok {
		t.Fatalf("expected ParseComplete, got %T", msg)
	}
}

func expectBindComplete(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		t.Fatalf("expected BindComplete, got ErrorResponse code=%s severity=%s message=%s", errResp.Code, errResp.Severity, errResp.Message)
	}
	if _, ok := msg.(*pgproto3.BindComplete); !ok {
		t.Fatalf("expected BindComplete, got %T", msg)
	}
}

func expectRowDescriptionInt4(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	rd, ok := msg.(*pgproto3.RowDescription)
	if !ok {
		t.Fatalf("expected RowDescription, got %T", msg)
	}
	if len(rd.Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(rd.Fields))
	}
	const int4OID = 23
	if rd.Fields[0].DataTypeOID != int4OID {
		t.Fatalf("expected int4 OID %d, got %d", int4OID, rd.Fields[0].DataTypeOID)
	}
}

func expectRowDescriptionInt4Text(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	rd, ok := msg.(*pgproto3.RowDescription)
	if !ok {
		t.Fatalf("expected RowDescription, got %T", msg)
	}
	if len(rd.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(rd.Fields))
	}
	const int4OID = 23
	const textOID = 25
	if rd.Fields[0].DataTypeOID != int4OID {
		t.Fatalf("expected int4 OID %d, got %d", int4OID, rd.Fields[0].DataTypeOID)
	}
	if rd.Fields[1].DataTypeOID != textOID {
		t.Fatalf("expected text OID %d, got %d", textOID, rd.Fields[1].DataTypeOID)
	}
}

func expectParameterDescriptionInt4(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		t.Fatalf("expected ParameterDescription, got ErrorResponse code=%s severity=%s message=%s", errResp.Code, errResp.Severity, errResp.Message)
	}
	pd, ok := msg.(*pgproto3.ParameterDescription)
	if !ok {
		t.Fatalf("expected ParameterDescription, got %T", msg)
	}
	if len(pd.ParameterOIDs) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(pd.ParameterOIDs))
	}
	const int4OID = 23
	if pd.ParameterOIDs[0] != int4OID {
		t.Fatalf("expected int4 OID %d, got %d", int4OID, pd.ParameterOIDs[0])
	}
}

func expectDataRowValues(t *testing.T, msg pgproto3.BackendMessage, values ...string) {
	t.Helper()
	dr, ok := msg.(*pgproto3.DataRow)
	if !ok {
		t.Fatalf("expected DataRow, got %T", msg)
	}
	if len(dr.Values) != len(values) {
		t.Fatalf("expected %d values, got %d", len(values), len(dr.Values))
	}
	for i, value := range values {
		if string(dr.Values[i]) != value {
			t.Fatalf("expected value %q at %d, got %q", value, i, dr.Values[i])
		}
	}
}

func expectErrorResponseCode(t *testing.T, msg pgproto3.BackendMessage, code string) {
	t.Helper()
	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got %T", msg)
	}
	if errResp.Code != code {
		t.Fatalf("expected error code %q, got %q", code, errResp.Code)
	}
}

func expectCommandComplete(t *testing.T, msg pgproto3.BackendMessage, tag string) {
	t.Helper()
	cc, ok := msg.(*pgproto3.CommandComplete)
	if !ok {
		t.Fatalf("expected CommandComplete, got %T", msg)
	}
	if string(cc.CommandTag) != tag {
		t.Fatalf("expected command tag %q, got %q", tag, cc.CommandTag)
	}
}

func expectPortalSuspended(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	if _, ok := msg.(*pgproto3.PortalSuspended); !ok {
		t.Fatalf("expected PortalSuspended, got %T", msg)
	}
}

func expectCloseComplete(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	if _, ok := msg.(*pgproto3.CloseComplete); !ok {
		t.Fatalf("expected CloseComplete, got %T", msg)
	}
}

func expectReadyForQuery(t *testing.T, msg pgproto3.BackendMessage, status byte) *pgproto3.ReadyForQuery {
	t.Helper()
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
		t.Fatalf("expected ReadyForQuery, got ErrorResponse code=%s severity=%s message=%s", errResp.Code, errResp.Severity, errResp.Message)
	}
	rfq, ok := msg.(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("expected ReadyForQuery, got %T", msg)
	}
	if rfq.TxStatus != status {
		t.Fatalf("expected TxStatus %q, got %q", status, rfq.TxStatus)
	}
	return rfq
}
