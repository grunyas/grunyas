package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
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

	viper.SetDefault("server.ssl_mode", "never")
	viper.SetDefault("server.ssl_cert", "")
	viper.SetDefault("server.ssl_key", "")

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
	keys := []string{
		"server.listen_addr",
		"server.admin_addr",
		"server.max_sessions",
		"server.client_idle_timeout",
		"server.keep_alive_timeout",
		"server.keep_alive_interval",
		"server.keep_alive_count",
		"server.ssl_mode",
		"server.ssl_cert",
		"server.ssl_key",
		"server.pool_mode",
		"logging.level",
		"logging.development",
		"telemetry.otlp_endpoint",
		"telemetry.insecure",
		"telemetry.service_name",
		"auth.method",
		"auth.username",
		"auth.password",
		"backend.host",
		"backend.port",
		"backend.user",
		"backend.password",
		"backend.database",
		"backend.connect_timeout_seconds",
		"backend.pool_min_conns",
		"backend.pool_max_conns",
		"backend.pool_max_conn_lifetime",
		"backend.pool_max_conn_idle_time",
		"backend.pool_health_check_period",
	}

	for _, key := range keys {
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
