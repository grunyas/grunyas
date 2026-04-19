package downstream_client

import (
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

func TestHandshakeSendsWelcomeSequence(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	c := Initialize(serverConn, nil, false, zap.NewNop())

	errCh := make(chan error, 1)
	go func() { errCh <- c.Handshake() }()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	var paramCount int
	var gotKeyData, gotRFQ bool
	for i := 0; i < 8; i++ {
		msg, err := frontend.Receive()
		if err != nil {
			t.Fatalf("receive msg %d: %v", i, err)
		}
		switch m := msg.(type) {
		case *pgproto3.ParameterStatus:
			paramCount++
		case *pgproto3.BackendKeyData:
			gotKeyData = true
			if m.ProcessID != 1234 {
				t.Errorf("expected ProcessID 1234, got %d", m.ProcessID)
			}
		case *pgproto3.ReadyForQuery:
			gotRFQ = true
			if m.TxStatus != 'I' {
				t.Errorf("expected TxStatus 'I', got '%c'", m.TxStatus)
			}
		default:
			t.Fatalf("unexpected message type: %T", msg)
		}
	}

	if paramCount != 6 {
		t.Errorf("expected 6 ParameterStatus, got %d", paramCount)
	}
	if !gotKeyData {
		t.Error("expected BackendKeyData")
	}
	if !gotRFQ {
		t.Error("expected ReadyForQuery")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Handshake failed: %v", err)
	}
}

func TestSASLExchangeFullFlow(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	c := Initialize(serverConn, nil, false, zap.NewNop())

	var stepCalls []string
	stepFn := func(input string) (string, error) {
		stepCalls = append(stepCalls, input)
		return fmt.Sprintf("response-%d", len(stepCalls)), nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- c.SASLExchange(stepFn) }()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	// Receive AuthenticationSASL
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("receive AuthenticationSASL: %v", err)
	}
	sasl, ok := msg.(*pgproto3.AuthenticationSASL)
	if !ok {
		t.Fatalf("expected AuthenticationSASL, got %T", msg)
	}
	if len(sasl.AuthMechanisms) != 1 || sasl.AuthMechanisms[0] != "SCRAM-SHA-256" {
		t.Errorf("unexpected mechanisms: %v", sasl.AuthMechanisms)
	}

	// Send SASLInitialResponse
	frontend.Send(&pgproto3.SASLInitialResponse{AuthMechanism: "SCRAM-SHA-256", Data: []byte("client-first")})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SASLInitialResponse: %v", err)
	}

	// Receive AuthenticationSASLContinue with server-first message
	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("receive AuthenticationSASLContinue: %v", err)
	}
	cont, ok := msg.(*pgproto3.AuthenticationSASLContinue)
	if !ok {
		t.Fatalf("expected AuthenticationSASLContinue, got %T", msg)
	}
	if string(cont.Data) != "response-1" {
		t.Errorf("expected server-first 'response-1', got %q", cont.Data)
	}

	// Send SASLResponse
	frontend.Send(&pgproto3.SASLResponse{Data: []byte("client-final")})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush SASLResponse: %v", err)
	}

	// Receive AuthenticationSASLFinal with server-final message
	msg, err = frontend.Receive()
	if err != nil {
		t.Fatalf("receive AuthenticationSASLFinal: %v", err)
	}
	final, ok := msg.(*pgproto3.AuthenticationSASLFinal)
	if !ok {
		t.Fatalf("expected AuthenticationSASLFinal, got %T", msg)
	}
	if string(final.Data) != "response-2" {
		t.Errorf("expected server-final 'response-2', got %q", final.Data)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("SASLExchange failed: %v", err)
	}
	if len(stepCalls) != 2 {
		t.Errorf("expected 2 stepFn calls, got %d: %v", len(stepCalls), stepCalls)
	}
	if stepCalls[0] != "client-first" {
		t.Errorf("expected first step input 'client-first', got %q", stepCalls[0])
	}
	if stepCalls[1] != "client-final" {
		t.Errorf("expected second step input 'client-final', got %q", stepCalls[1])
	}
}

func TestSendAndFlushBatches(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	c := Initialize(serverConn, nil, false, zap.NewNop())

	if err := c.Send(
		&pgproto3.ParameterStatus{Name: "key1", Value: "val1"},
		&pgproto3.ParameterStatus{Name: "key2", Value: "val2"},
	); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- c.Flush() }()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)

	for i, want := range []string{"key1", "key2"} {
		msg, err := frontend.Receive()
		if err != nil {
			t.Fatalf("receive msg %d: %v", i, err)
		}
		ps, ok := msg.(*pgproto3.ParameterStatus)
		if !ok {
			t.Fatalf("msg %d: expected ParameterStatus, got %T", i, msg)
		}
		if ps.Name != want {
			t.Errorf("msg %d: expected name %q, got %q", i, want, ps.Name)
		}
	}

	if err := <-errCh; err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
}

func TestComputeMD5Password(t *testing.T) {
	// PostgreSQL MD5 password: "md5" + md5(md5(password + user) + salt)
	// We can verify against known values.
	user := "postgres"
	password := "secret"
	salt := [4]byte{0x01, 0x02, 0x03, 0x04}

	result := ComputeMD5Password(user, password, salt)

	if result[:3] != "md5" {
		t.Fatalf("expected result to start with 'md5', got %q", result)
	}
	// MD5 hex is 32 chars + "md5" prefix = 35
	if len(result) != 35 {
		t.Fatalf("expected 35 chars, got %d", len(result))
	}

	// Same inputs should produce same output (deterministic)
	result2 := ComputeMD5Password(user, password, salt)
	if result != result2 {
		t.Fatalf("expected deterministic output, got %q and %q", result, result2)
	}

	// Different salt should produce different output
	salt2 := [4]byte{0x05, 0x06, 0x07, 0x08}
	result3 := ComputeMD5Password(user, password, salt2)
	if result == result3 {
		t.Fatalf("expected different result for different salt")
	}

	// Different user should produce different output
	result4 := ComputeMD5Password("other", password, salt)
	if result == result4 {
		t.Fatalf("expected different result for different user")
	}
}
