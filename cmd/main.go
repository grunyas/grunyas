package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/console"
	"github.com/grunyas/grunyas/internal/logger"
	"github.com/grunyas/grunyas/internal/server/proxy"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

func main() {
	noConsole := flag.Bool("no-console", false, "run without the interactive console")
	flag.Parse()

	cfg := config.Default()

	viper.SetEnvPrefix("GRUNYAS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	bindEnvKeys()

	viper.SetConfigType("toml")
	viper.SetConfigName("config")
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			panic(fmt.Errorf("fatal error config file: %s", err))
		}
	}

	if err := viper.Unmarshal(&cfg); err != nil {
		panic(fmt.Errorf("failed to unmarshal config: %s", err))
	}

	cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("invalid configuration: %v", err))
	}

	ctx := withSignalContext()

	// Create a channel for logs only if using console
	var logCh *logger.LoggerChannel
	var logWriter io.Writer // Use io.Writer to properly pass nil
	if !*noConsole {
		logCh = logger.NewLoggerChannel()
		logWriter = logCh
	}

	logger, cleanup, err := logger.Initialize(ctx, cfg.Logging, cfg.Telemetry, logWriter)
	if err != nil {
		panic(fmt.Errorf("failed to initialize logging/telemetry: %w", err))
	}
	defer cleanup(context.Background()) //nolint:errcheck

	if addr := cfg.ServerConfig.PprofAddr; addr != "" {
		runtime.SetBlockProfileRate(1)     // record every block event
		runtime.SetMutexProfileFraction(1) // record every mutex contention event
		go func() {
			logger.Info("pprof server listening", zap.String("addr", addr))
			if err := http.ListenAndServe(addr, nil); err != nil {
				logger.Warn("pprof server error", zap.Error(err))
			}
		}()
	}

	srv := proxy.Initialize(ctx, &cfg, logger)

	// Run server in background (since it blocks)
	go func() {
		if err := srv.Run(); err != nil {
			logger.Panic("server error", zap.Error(err))
		}
	}()

	if *noConsole {
		<-ctx.Done()
		return
	}

	// Start interactive console in main thread (blocks until quit)
	console.Start(ctx, srv, logCh.Channel)
}

func bindEnvKeys() {
	// Bind server config (non-port fields)
	serverKeys := []string{
		"server.admin_addr",
		"server.max_sessions",
		"server.client_idle_timeout",
		"server.keep_alive_timeout",
		"server.keep_alive_interval",
		"server.keep_alive_count",
		"server.startup_probe_timeout_seconds",
		"server.pprof_addr",
	}

	// Bind write port config
	portKeys := []string{
		"server.ports.write.listen_addr",
		"server.ports.write.pool_mode",
		"server.ports.write.ssl_mode",
		"server.ports.write.ssl_cert",
		"server.ports.write.ssl_key",
	}

	// Bind probe config
	probeKeys := []string{
		"probe.interval_ms",
		"probe.liveness_failure_count",
		"probe.liveness_max_age_ms",
		"probe.role_max_age_ms",
	}

	// Bind logging, telemetry, auth
	otherKeys := []string{
		"logging.level",
		"logging.development",
		"telemetry.otlp_endpoint",
		"telemetry.insecure",
		"telemetry.service_name",
		"auth.method",
		"auth.username",
		"auth.password",
	}

	allKeys := append(append(append(serverKeys, portKeys...), probeKeys...), otherKeys...)
	for _, key := range allKeys {
		if err := viper.BindEnv(key); err != nil {
			panic(fmt.Errorf("failed to bind env for %s: %w", key, err))
		}
	}

	// Bind indexed node configs. Env-var override is capped at envNodeMax
	// nodes (i.e. GRUNYAS_NODES_0_* through GRUNYAS_NODES_{envNodeMax-1}_*).
	// Nodes declared beyond this cap in TOML still load; only env override
	// stops working past the cap. Documented in config.toml.example.
	for i := 0; i < envNodeMax; i++ {
		bindNodeEnvVars(i)
	}
}

// envNodeMax caps how many node indices accept env-var overrides.
// Nodes declared in TOML are unaffected.
const envNodeMax = 10

// bindNodeEnvVars binds environment variables for a single node at the given index.
func bindNodeEnvVars(index int) {
	prefix := fmt.Sprintf("nodes.%d", index)

	nodeKeys := []string{
		fmt.Sprintf("%s.id", prefix),
		fmt.Sprintf("%s.host", prefix),
		fmt.Sprintf("%s.port", prefix),
		fmt.Sprintf("%s.declared_role", prefix),
	}

	connKeys := []string{
		fmt.Sprintf("%s.connection.user", prefix),
		fmt.Sprintf("%s.connection.password", prefix),
		fmt.Sprintf("%s.connection.database", prefix),
		fmt.Sprintf("%s.connection.connect_timeout_seconds", prefix),
		fmt.Sprintf("%s.connection.ssl_mode", prefix),
	}

	poolKeys := []string{
		fmt.Sprintf("%s.pool.min_conns", prefix),
		fmt.Sprintf("%s.pool.max_conns", prefix),
		fmt.Sprintf("%s.pool.max_conn_lifetime", prefix),
		fmt.Sprintf("%s.pool.max_conn_idle_time", prefix),
		fmt.Sprintf("%s.pool.health_check_period", prefix),
	}

	allKeys := append(append(nodeKeys, connKeys...), poolKeys...)
	for _, key := range allKeys {
		if err := viper.BindEnv(key); err != nil {
			panic(fmt.Errorf("failed to bind env for %s: %w", key, err))
		}
	}
}

func withSignalContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)

	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-ch
		log.Printf("received signal %s, shutting down", sig)
		cancel()
	}()

	return ctx
}
