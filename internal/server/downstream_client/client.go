package downstream_client

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"sync"

	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

type Client struct {
	conn    net.Conn
	backend *pgproto3.Backend
	logger  *zap.Logger
	mu      sync.Mutex

	requiredSSL bool
	tlsConfig   *tls.Config
	md5Salt     [4]byte
}

func Initialize(conn net.Conn, tlsConfig *tls.Config, requiredSSL bool, logger *zap.Logger) *Client {
	return &Client{
		backend:     pgproto3.NewBackend(conn, conn),
		conn:        conn,
		logger:      logger,
		requiredSSL: requiredSSL,
		tlsConfig:   tlsConfig,
	}
}

// Startup handles the initial connection sequence (SSL and Authentication).
// The authMethod parameter determines what challenge is sent to the client.
// For AuthPlain: returns (user, cleartext_password, nil)
// For AuthMD5: returns (user, md5_hashed_password, nil)
// For AuthScramSHA256: returns (user, "", nil) — SASL exchange is done separately via SASLExchange.
func (c *Client) Startup(authMethod types.AuthMethod) (string, string, error) {
	for {
		msg, err := c.backend.ReceiveStartupMessage()
		if err != nil {
			return "", "", err
		}

		switch m := msg.(type) {
		case *pgproto3.SSLRequest:
			c.logger.Debug("ssl request message received")

			if c.tlsConfig != nil {
				if _, err := c.conn.Write([]byte("S")); err != nil {
					return "", "", err
				}

				c.logger.Debug("upgrading connection to tls")
				tlsConn := tls.Server(c.conn, c.tlsConfig)

				if err := tlsConn.Handshake(); err != nil {
					return "", "", fmt.Errorf("tls handshake failed: %w", err)
				}

				c.conn = tlsConn
				c.backend = pgproto3.NewBackend(c.conn, c.conn)
			} else {
				c.logger.Debug("rejecting ssl request")
				if _, err := c.conn.Write([]byte("N")); err != nil {
					return "", "", err
				}
			}
			continue
		case *pgproto3.GSSEncRequest:
			if _, err := c.conn.Write([]byte("N")); err != nil {
				return "", "", err
			}
			continue
		case *pgproto3.StartupMessage:
			// Enforce Mandatory SSL
			if c.requiredSSL {
				if _, ok := c.conn.(*tls.Conn); !ok {
					if err := c.Send(&pgproto3.ErrorResponse{
						Severity: "FATAL",
						Code:     "28000",
						Message:  "SSL connection is required",
					}); err != nil {
						return "", "", err
					}

					return "", "", fmt.Errorf("ssl connection is required")
				}
			}

			user := m.Parameters["user"]

			switch authMethod {
			case types.AuthMD5:
				return c.startupMD5(user)
			case types.AuthScramSHA256:
				// For SCRAM, we return just the user. The SASL exchange
				// is handled separately via SASLExchange().
				return user, "", nil
			default:
				return c.startupCleartext(user)
			}
		default:
			return "", "", fmt.Errorf("unexpected message type: %T", msg)
		}
	}
}

// startupCleartext sends an AuthenticationCleartextPassword challenge and reads the response.
func (c *Client) startupCleartext(user string) (string, string, error) {
	if err := c.Send(&pgproto3.AuthenticationCleartextPassword{}); err != nil {
		return "", "", err
	}

	msg, err := c.backend.Receive()
	if err != nil {
		return "", "", err
	}

	pw, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		return "", "", fmt.Errorf("expected password message, got %T", msg)
	}

	return user, pw.Password, nil
}

// startupMD5 sends an AuthenticationMD5Password challenge with a random salt
// and reads the hashed response.
func (c *Client) startupMD5(user string) (string, string, error) {
	if _, err := rand.Read(c.md5Salt[:]); err != nil {
		return "", "", fmt.Errorf("generate MD5 salt: %w", err)
	}

	if err := c.Send(&pgproto3.AuthenticationMD5Password{Salt: c.md5Salt}); err != nil {
		return "", "", err
	}

	msg, err := c.backend.Receive()
	if err != nil {
		return "", "", err
	}

	pw, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		return "", "", fmt.Errorf("expected password message, got %T", msg)
	}

	return user, pw.Password, nil
}

// MD5Salt returns the salt used in the most recent MD5 authentication challenge.
func (c *Client) MD5Salt() [4]byte {
	return c.md5Salt
}

// SASLExchange performs the full SASL/SCRAM-SHA-256 handshake with the client.
// stepFn is called for each step of the SCRAM conversation (typically ScramSession.Step).
func (c *Client) SASLExchange(stepFn func(string) (string, error)) error {
	// Step 1: Send AuthenticationSASL with supported mechanisms.
	if err := c.Send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}}); err != nil {
		return fmt.Errorf("send AuthenticationSASL: %w", err)
	}

	// Step 2: Receive SASLInitialResponse from client.
	msg, err := c.backend.Receive()
	if err != nil {
		return fmt.Errorf("receive SASLInitialResponse: %w", err)
	}
	initial, ok := msg.(*pgproto3.SASLInitialResponse)
	if !ok {
		return fmt.Errorf("expected SASLInitialResponse, got %T", msg)
	}

	// Step 3: Process client-first message, get server-first message.
	serverFirst, err := stepFn(string(initial.Data))
	if err != nil {
		return fmt.Errorf("SCRAM step 1: %w", err)
	}

	if err := c.Send(&pgproto3.AuthenticationSASLContinue{Data: []byte(serverFirst)}); err != nil {
		return fmt.Errorf("send AuthenticationSASLContinue: %w", err)
	}

	// Step 4: Receive SASLResponse (client-final message).
	msg, err = c.backend.Receive()
	if err != nil {
		return fmt.Errorf("receive SASLResponse: %w", err)
	}
	response, ok := msg.(*pgproto3.SASLResponse)
	if !ok {
		return fmt.Errorf("expected SASLResponse, got %T", msg)
	}

	// Step 5: Process client-final, get server-final.
	serverFinal, err := stepFn(string(response.Data))
	if err != nil {
		return fmt.Errorf("SCRAM step 2: %w", err)
	}

	if err := c.Send(&pgproto3.AuthenticationSASLFinal{Data: []byte(serverFinal)}); err != nil {
		return fmt.Errorf("send AuthenticationSASLFinal: %w", err)
	}

	return nil
}

// ComputeMD5Password computes the PostgreSQL MD5 password hash:
// "md5" + md5(md5(password + user) + salt)
func ComputeMD5Password(user, password string, salt [4]byte) string {
	// Phase 1: md5(password + user)
	h1 := md5.New()
	h1.Write([]byte(password))
	h1.Write([]byte(user))
	inner := hex.EncodeToString(h1.Sum(nil))

	// Phase 2: md5(inner + salt)
	h2 := md5.New()
	h2.Write([]byte(inner))
	h2.Write(salt[:])
	return "md5" + hex.EncodeToString(h2.Sum(nil))
}

func (c *Client) Handshake() error {
	// Send ParameterStatus messages
	ps := []pgproto3.BackendMessage{
		&pgproto3.ParameterStatus{Name: "server_version", Value: "14.0"}, // Using a standard version
		&pgproto3.ParameterStatus{Name: "server_encoding", Value: "UTF8"},
		&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"},
		&pgproto3.ParameterStatus{Name: "DateStyle", Value: "ISO, MDY"},
		&pgproto3.ParameterStatus{Name: "integer_datetimes", Value: "on"},
		&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"},
	}

	if err := c.Send(ps...); err != nil {
		return fmt.Errorf("failed to send parameter status: %w", err)
	}

	// Send BackendKeyData (dummy for now)
	if err := c.Send(&pgproto3.BackendKeyData{ProcessID: 1234, SecretKey: 5678}); err != nil {
		return fmt.Errorf("failed to send backend key data: %w", err)
	}

	// Send ReadyForQuery
	if err := c.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'}); err != nil {
		return fmt.Errorf("failed to send ready for query: %w", err)
	}

	return nil
}

func (c *Client) Receive() (pgproto3.FrontendMessage, error) {
	return c.backend.Receive()
}

func (c *Client) Send(msgs ...pgproto3.BackendMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, msg := range msgs {
		c.backend.Send(msg)
	}
	return c.backend.Flush()
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}
