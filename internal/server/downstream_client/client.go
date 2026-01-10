package downstream_client

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"

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
// It returns the username and password provided by the client.
func (c *Client) Startup() (string, string, error) {
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

			// Request cleartext password
			if err := c.Send(&pgproto3.AuthenticationCleartextPassword{}); err != nil {
				return "", "", err
			}

			// Receive password message
			msg, err := c.backend.Receive()
			if err != nil {
				return "", "", err
			}

			pw, ok := msg.(*pgproto3.PasswordMessage)
			if !ok {
				return "", "", fmt.Errorf("expected password message, got %T", msg)
			}

			return user, pw.Password, nil
		default:
			return "", "", fmt.Errorf("unexpected message type: %T", msg)
		}
	}
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
