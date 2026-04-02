package auth

import (
	"testing"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/server/downstream_client"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/xdg-go/scram"
	"go.uber.org/zap"
)

func newAuthenticator(t *testing.T, method, password string) *Authenticator {
	t.Helper()
	auth, err := Initialize(config.AuthConfig{
		Method:   method,
		Username: "postgres",
		Password: password,
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	return auth
}

func TestMethodMapping(t *testing.T) {
	tests := []struct {
		method string
		want   types.AuthMethod
	}{
		{"plain", types.AuthPlain},
		{"md5", types.AuthMD5},
		{"scram-sha-256", types.AuthScramSHA256},
		{"", types.AuthPlain},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			auth := newAuthenticator(t, tt.method, "secret")
			if got := auth.Method(); got != tt.want {
				t.Fatalf("Method() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthenticatePlain(t *testing.T) {
	auth := newAuthenticator(t, "plain", "secret")

	if err := auth.Authenticate("postgres", "secret"); err != nil {
		t.Fatalf("valid credentials should succeed: %v", err)
	}
	if err := auth.Authenticate("postgres", "wrong"); err == nil {
		t.Fatal("wrong password should fail")
	}
	if err := auth.Authenticate("unknown", "secret"); err == nil {
		t.Fatal("unknown user should fail")
	}
}

func TestAuthenticateMD5(t *testing.T) {
	auth := newAuthenticator(t, "md5", "secret")

	salt := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	expected := downstream_client.ComputeMD5Password("postgres", "secret", salt)

	if err := auth.AuthenticateMD5("postgres", expected, salt); err != nil {
		t.Fatalf("valid MD5 hash should succeed: %v", err)
	}
	if err := auth.AuthenticateMD5("postgres", "md5wrong", salt); err == nil {
		t.Fatal("wrong MD5 hash should fail")
	}
	if err := auth.AuthenticateMD5("unknown", expected, salt); err == nil {
		t.Fatal("unknown user should fail")
	}
}

func TestNewSCRAMSessionFailsWhenNotConfigured(t *testing.T) {
	auth := newAuthenticator(t, "plain", "secret")

	_, err := auth.NewSCRAMSession()
	if err == nil {
		t.Fatal("NewSCRAMSession should fail when not configured for SCRAM")
	}
}

func TestSCRAMFullExchange(t *testing.T) {
	auth := newAuthenticator(t, "scram-sha-256", "secret")

	serverSession, err := auth.NewSCRAMSession()
	if err != nil {
		t.Fatalf("NewSCRAMSession failed: %v", err)
	}

	// Create a SCRAM client to drive the exchange.
	client, err := scram.SHA256.NewClient("postgres", "secret", "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	conv := client.NewConversation()

	// Step 1: client-first
	clientFirst, err := conv.Step("")
	if err != nil {
		t.Fatalf("client step 1 failed: %v", err)
	}

	// Step 2: server processes client-first, returns server-first
	serverFirst, err := serverSession.Step(clientFirst)
	if err != nil {
		t.Fatalf("server step 1 failed: %v", err)
	}

	// Step 3: client processes server-first, returns client-final
	clientFinal, err := conv.Step(serverFirst)
	if err != nil {
		t.Fatalf("client step 2 failed: %v", err)
	}

	// Step 4: server processes client-final, returns server-final
	serverFinal, err := serverSession.Step(clientFinal)
	if err != nil {
		t.Fatalf("server step 2 failed: %v", err)
	}

	// Step 5: client validates server-final
	_, err = conv.Step(serverFinal)
	if err != nil {
		t.Fatalf("client step 3 (validation) failed: %v", err)
	}

	if !conv.Valid() {
		t.Fatal("expected SCRAM conversation to be valid")
	}
}

func TestSCRAMExchangeWrongPassword(t *testing.T) {
	auth := newAuthenticator(t, "scram-sha-256", "secret")

	serverSession, err := auth.NewSCRAMSession()
	if err != nil {
		t.Fatalf("NewSCRAMSession failed: %v", err)
	}

	// Create a SCRAM client with the wrong password.
	client, err := scram.SHA256.NewClient("postgres", "wrongpassword", "")
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	if err != nil {
		t.Fatalf("client step 1 failed: %v", err)
	}

	serverFirst, err := serverSession.Step(clientFirst)
	if err != nil {
		t.Fatalf("server step 1 failed: %v", err)
	}

	clientFinal, err := conv.Step(serverFirst)
	if err != nil {
		t.Fatalf("client step 2 failed: %v", err)
	}

	// Server should reject the wrong proof
	_, err = serverSession.Step(clientFinal)
	if err == nil {
		t.Fatal("expected SCRAM exchange to fail with wrong password")
	}
}
