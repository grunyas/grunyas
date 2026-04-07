package scenarios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Config holds the connection and concurrency settings for scenario execution.
type Config struct {
	ConnStr     string
	Concurrency int
	PoolMode    string
	DBHost      string
	DBPort      string
	DBUser      string
	DBPass      string
	DBName      string
}

// Result captures the output of a single scenario run.
type Result struct {
	TotalOps  int
	Errors    int
	Duration  time.Duration
	Latencies []time.Duration
	Notes     []string
}

// ErrorSampler collects up to maxSamples unique error messages for diagnostics.
type ErrorSampler struct {
	mu      sync.Mutex
	seen    map[string]int // error message → count
	samples []string       // first maxSamples unique messages
	max     int
}

func NewErrorSampler(maxSamples int) *ErrorSampler {
	return &ErrorSampler{
		seen: make(map[string]int),
		max:  maxSamples,
	}
}

// Record adds an error. Returns true so callers can use it inline.
func (es *ErrorSampler) Record(err error) bool {
	msg := err.Error()
	es.mu.Lock()
	es.seen[msg]++
	if es.seen[msg] == 1 && len(es.samples) < es.max {
		es.samples = append(es.samples, msg)
	}
	es.mu.Unlock()
	return true
}

// Notes returns formatted error sample lines for the Result.Notes field.
func (es *ErrorSampler) Notes() []string {
	es.mu.Lock()
	defer es.mu.Unlock()
	var notes []string
	for _, msg := range es.samples {
		notes = append(notes, fmt.Sprintf("[%dx] %s", es.seen[msg], msg))
	}
	return notes
}

// IsCapacityError checks if an error is a capacity rejection.
// PostgreSQL uses SQLSTATE 53300 (too_many_connections).
// PgBouncer uses SQLSTATE 08P01 with message "no more connections allowed (max_client_conn)".
// These are expected in connection_storms which intentionally exceeds the configured limit.
func IsCapacityError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "53300" {
			return true
		}
		if pgErr.Code == "08P01" && strings.Contains(pgErr.Message, "no more connections allowed") {
			return true
		}
	}
	return false
}

// ConnectClient opens a single connection simulating a real application client.
// Each caller holds this connection for its entire lifetime — connect once, do all
// the work, then close. This mirrors how real application threads/goroutines behave
// when talking directly to a database or through a connection pooler.
//
// In transaction pool mode, QueryExecModeCacheDescribe is used so that after the first
// execution of each unique query the description is cached locally. Subsequent executions
// send only Bind+Execute as a single round-trip, safe across backend switches.
func ConnectClient(ctx context.Context, cfg *Config) (*pgx.Conn, error) {
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		cfg.DBUser, cfg.DBPass, cfg.DBHost, cfg.DBPort, cfg.DBName)
	connCfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.PoolMode == "transaction" {
		connCfg.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	}
	return pgx.ConnectConfig(ctx, connCfg)
}
