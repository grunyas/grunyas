// Package types defines shared interfaces and types used across the server components.
package types

import (
	"context"
	"time"

	"github.com/grunyas/grunyas/config"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

// ProxyInterface defines the interface for the main proxy server functionality
// exposed to other components like sessions.
// It allows access to shared resources such as the connection pool, context, and logger.
type ProxyInterface interface {
	// GetContext returns the server's base context.
	GetContext() context.Context

	// GetConfig returns the server's configuration.
	GetConfig() *config.Config

	// GetLogger returns the server's logger.
	GetLogger() *zap.Logger

	// PoolStats returns the current statistics of the database connection pool.
	PoolStats() PoolStats

	// AuthenticateUser validates the user credentials.
	AuthenticateUser(user, password string) error

	// AcquireUpstream obtains a connection from the pool.
	AcquireUpstream() (UpstreamClientInterface, error)
}

// PoolStats represents the current statistics of the database connection pool.
type PoolStats struct {
	TotalConns    int32
	AcquiredConns int32
	IdleConns     int32
	MaxConns      int32
}

// PoolManagerInterface defines the interface for the database connection pool manager.
type PoolManagerInterface interface {
	// AcquireDbConnection obtains a connection from the database pool.
	AcquireDbConnection() (UpstreamClientInterface, error)

	// PoolStats returns the current statistics of the database connection pool.
	PoolStats() PoolStats

	// Close closes the connection pool.
	Close()
}

// UpstreamClientInterface defines the interface for the upstream database client.
type UpstreamClientInterface interface {
	// Send sends the given messages to the database.
	Send(...pgproto3.FrontendMessage) error

	// Receive reads a message from the upstream connection.
	Receive(ctx context.Context) (pgproto3.BackendMessage, error)

	// TxStatus returns the current transaction status of the upstream connection.
	TxStatus() byte

	// Release releases the connection back to the pool.
	Release() error

	// Kill destroys the connection instead of returning it to the pool.
	Kill() error
}

// ResultReader defines the interface for streaming results from an upstream query.
type ResultReader interface {
	// NextResult advances to the next result set (if any).
	NextResult() bool

	// FieldDescriptions returns the description of the fields in the current result set.
	FieldDescriptions() []pgproto3.FieldDescription

	// NextRow advances to the next row in the current result set.
	NextRow() bool

	// Values returns the raw byte values for the current row.
	Values() [][]byte

	// Close closes the reader and returns the command tag and any error encountered.
	Close() (pgproto3.CommandComplete, error)
}

// DownstreamClientInterface defines the interface for the downstream client connection.
type DownstreamClientInterface interface {
	// Startup handles the initial connection sequence including SSL negotiation and authentication.
	// It returns the username and password provided by the client.
	Startup() (string, string, error)

	// Handshake performs the post-authentication initialization (parameter status, etc).
	Handshake() error

	// Receive reads a message from the client.
	Receive() (pgproto3.FrontendMessage, error)

	// Send sends messages to the client.
	Send(...pgproto3.BackendMessage) error

	// Close closes the client connection.
	Close() error
}

// Expirable defines the interface for resources that can be tracked and expired by the idle sweeper.
type Expirable interface {
	// ID returns a unique identifier for the expirable resource.
	ID() uint64

	// LastActive returns the time of the most recent activity.
	LastActive() time.Time

	// CloseWithError terminates the resource with a PostgreSQL error response.
	CloseWithError(severity, code, message string) error
}

// Error types for distinguishing between different failure modes during connection setup.
var (
	// ErrAuthFailed indicates that authentication was unsuccessful.
	ErrAuthFailed = &ProxyError{Code: "28P01", Message: "invalid password"}

	// ErrPoolExhausted indicates that no database connections are available.
	ErrPoolExhausted = &ProxyError{Code: "53300", Message: "connection pool exhausted"}
)

// ProxyError represents a PostgreSQL error with a specific error code.
type ProxyError struct {
	Code    string
	Message string
}

func (e *ProxyError) Error() string {
	return e.Message
}
