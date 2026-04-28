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
	"time"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/console"
	"github.com/grunyas/grunyas/internal/logger"
	"github.com/grunyas/grunyas/internal/server/proxy"
	"github.com/grunyas/grunyas/internal/topology"
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
	var logWriter io.Writer
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
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
		go func() {
			logger.Info("pprof server listening", zap.String("addr", addr))
			if err := http.ListenAndServe(addr, nil); err != nil {
				logger.Warn("pprof server error", zap.Error(err))
			}
		}()
	}

	// -----------------------------------------------------------------------
	// M1 startup flow (§5 of architecture/M1.md)
	// -----------------------------------------------------------------------

	// 3. Build topology object
	topo, err := topology.New(ctx, &cfg, logger)
	if err != nil {
		logger.Panic("failed to build topology", zap.Error(err))
	}
	defer topo.Close()

	// 5. Wait for every node's first probe to complete
	probeTimeout := time.Duration(cfg.ServerConfig.StartupProbeTimeoutSeconds) * time.Second
	if probeTimeout <= 0 {
		probeTimeout = 10 * time.Second
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
	topo.WaitForInitialProbes(probeCtx)
	probeCancel()

	// 6. Verify system_identifier consistency
	// Single-node deployments skip this check.
	if len(cfg.Nodes) > 1 {
		if sysIDErr := topo.SystemIDError(); sysIDErr != nil {
			logger.Panic("system_identifier mismatch detected during startup — aborting",
				zap.Error(sysIDErr))
		}
	}

	// All-nodes-down → abort (M1.md §5).
	// A node is "not up" if it never completed a probe (LastProbeAt.IsZero())
	// OR its last probe returned an error.
	allDown := true
	for _, nv := range topo.Nodes() {
		if !nv.LastProbeAt.IsZero() && nv.LastProbeErr == nil {
			allDown = false
			break
		}
	}
	if allDown {
		logger.Panic("all nodes unreachable at startup — aborting")
	}

	// Log unreachable nodes (partial-cluster startup is allowed).
	for _, nv := range topo.Nodes() {
		if nv.LastProbeErr != nil {
			logger.Warn("node unreachable at startup — starting as down",
				zap.String("node", string(nv.ID)),
				zap.Error(nv.LastProbeErr))
		}
	}

	// 7. Warn about unimplemented ports
	for portID := range cfg.ServerConfig.Ports {
		if portID != "write" {
			logger.Warn(fmt.Sprintf("port %q declared but not yet implemented in this version; ignored", portID))
		}
	}

	// 9. Admin port: serves /healthz only (M1 stub).
	if cfg.ServerConfig.AdminAddr != "" {
		adminMux := http.NewServeMux()
		adminMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"status":"ok"}`)
		})
		go func() {
			logger.Info("admin listener starting", zap.String("addr", cfg.ServerConfig.AdminAddr))
			if err := http.ListenAndServe(cfg.ServerConfig.AdminAddr, adminMux); err != nil {
				logger.Warn("admin listener error", zap.Error(err))
			}
		}()
	}

	// -----------------------------------------------------------------------
	// Serve
	// -----------------------------------------------------------------------

	srv := proxy.Initialize(ctx, &cfg, logger, topo)

	go func() {
		if err := srv.Run(); err != nil {
			logger.Panic("server error", zap.Error(err))
		}
	}()

	if *noConsole {
		<-ctx.Done()
		return
	}

	console.Start(ctx, srv, logCh.Channel)
}

func bindEnvKeys() {
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

	portKeys := []string{
		"server.ports.write.listen_addr",
		"server.ports.write.pool_mode",
		"server.ports.write.ssl_mode",
		"server.ports.write.ssl_cert",
		"server.ports.write.ssl_key",
	}

	probeKeys := []string{
		"probe.interval_ms",
		"probe.liveness_failure_count",
		"probe.liveness_max_age_ms",
		"probe.role_max_age_ms",
	}

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

	for i := 0; i < envNodeMax; i++ {
		bindNodeEnvVars(i)
	}
}

const envNodeMax = 10

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
