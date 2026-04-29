package proxy

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/auth"
	"github.com/grunyas/grunyas/internal/server/session"
)

func TestCanAcceptNewSessionRespectsMax(t *testing.T) {
	srv := newTestServer(t, func(c *config.Config) {
		c.ServerConfig.MaxSessions = 1
	})

	if got := srv.canAcceptNewConnection(); !got {
		t.Fatalf("expected first session to be accepted")
	}

	if got := srv.canAcceptNewConnection(); got {
		t.Fatalf("expected second session to be rejected when at max")
	}

	if cur := srv.currentConnectionsCount.Load(); cur != 1 {
		t.Fatalf("expected currentSessionsCount to remain at 1, got %d", cur)
	}
}

func newTestServer(t *testing.T, mutate func(*config.Config)) *Proxy {
	t.Helper()

	cfg := config.Config{
		ServerConfig: config.ServerConfig{
			ListenAddr:        "127.0.0.1:0",
			AdminAddr:         "127.0.0.1:0",
			MaxSessions:       100,
			ClientIdleTimeout: int((10 * time.Second).Seconds()),
			KeepAliveTimeout:  15,
			KeepAliveInterval: 15,
			KeepAliveCount:    9,
		},
		Logging: config.LoggingConfig{
			Level:       "info",
			Development: true,
		},
		Auth: config.AuthConfig{
			Method:   "plain",
			Username: "postgres",
			Password: "postgres",
		},
	}

	if mutate != nil {
		mutate(&cfg)
	}

	authn, err := auth.Initialize(cfg.Auth, zap.NewNop())
	if err != nil {
		t.Fatalf("failed to init auth: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Proxy{
		cfg:      &cfg,
		ctx:      ctx,
		auth:     authn,
		logger:   zap.NewNop(),
		sessions: make(map[*session.Session]struct{}),
		idle:     newIdleSweeper(time.Duration(cfg.ServerConfig.ClientIdleTimeout) * time.Second),
	}
}
