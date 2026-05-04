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
	"github.com/grunyas/grunyas/internal/admin"
	"github.com/grunyas/grunyas/internal/console"
	"github.com/grunyas/grunyas/internal/decisions"
	logpkg "github.com/grunyas/grunyas/internal/logger"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/routing"
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

	var logCh *logpkg.LoggerChannel
	var logWriter io.Writer
	if !*noConsole {
		logCh = logpkg.NewLoggerChannel()
		logWriter = logCh
	}

	logger, cleanup, err := logpkg.Initialize(ctx, cfg.Logging, cfg.Telemetry, logWriter)
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
	// M1 startup flow
	// -----------------------------------------------------------------------

	topo, err := topology.New(ctx, &cfg, logger)
	if err != nil {
		logger.Panic("failed to build topology", zap.Error(err))
	}
	defer topo.Close()

	probeTimeout := time.Duration(cfg.ServerConfig.StartupProbeTimeoutSeconds) * time.Second
	if probeTimeout <= 0 {
		probeTimeout = 10 * time.Second
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
	topo.WaitForInitialProbes(probeCtx)
	probeCancel()

	if len(cfg.Nodes) > 1 {
		if sysIDErr := topo.SystemIDError(); sysIDErr != nil {
			logger.Panic("system_identifier mismatch detected during startup — aborting",
				zap.Error(sysIDErr))
		}
	}

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

	for _, nv := range topo.Nodes() {
		if nv.LastProbeErr != nil {
			logger.Warn("node unreachable at startup — starting as down",
				zap.String("node", string(nv.ID)),
				zap.Error(nv.LastProbeErr))
		}
	}

	// -----------------------------------------------------------------------
	// M3: Policy engine
	// -----------------------------------------------------------------------

	templates := policy.NewTemplateSet()
	policyInstances := make([]policy.Instance, 0, len(cfg.Policies))
	for _, pc := range cfg.Policies {
		params := make(map[string]int)
		for k, v := range pc.Parameters {
			switch vv := v.(type) {
			case float64:
				params[k] = int(vv)
			case int:
				params[k] = vv
			}
		}
		policyInstances = append(policyInstances, policy.Instance{
			Name:       pc.Name,
			Template:   pc.Template,
			Scope:      pc.Scope,
			Parameters: params,
			Timing: policy.TemplateTiming{
				DwellMs:   pc.Timing.DwellMs,
				ReleaseMs: pc.Timing.ReleaseMs,
			},
		})
	}

	// -----------------------------------------------------------------------
	// M3: Decisions bus
	// -----------------------------------------------------------------------

	decisionsBus := decisions.NewBus(
		cfg.ServerConfig.Decisions.MaxSubscribers,
		cfg.ServerConfig.Decisions.PerSubscriberBuffer,
	)

	// -----------------------------------------------------------------------
	// M3: Policy engine (creation after bus, since bus is its notification channel)
	// -----------------------------------------------------------------------

	policyEng := policy.NewEngine(policyInstances, templates, logger)

	// -----------------------------------------------------------------------
	// M3: Routing pipeline
	// -----------------------------------------------------------------------

	routingPipeline := routing.NewPipeline(topo, policyEng, decisionsBus, logger)

	// Start observation-driven policy evaluation on the probe cadence.
	routingPipeline.StartObservationLoop(ctx, cfg.ProbeConfig.IntervalMs)

	// -----------------------------------------------------------------------
	// M3: OTel decision-log exporter (subscribes to bus, logs structured events)
	// -----------------------------------------------------------------------

	logpkg.StartDecisionExporter(ctx, decisionsBus, logger)

	// -----------------------------------------------------------------------
	// M2: Admin server
	// -----------------------------------------------------------------------

	adminSrv, err := admin.New(topo, &cfg, logger, decisionsBus, policyEng)
	if err != nil {
		logger.Panic("failed to initialize admin server", zap.Error(err))
	}

	adminSrv.SetRoutingMetrics(
		routingPipeline.PublishedTotal.Load,
		routingPipeline.EligibleSetRead.Load,
		routingPipeline.EligibleSetWrite.Load,
		routingPipeline.DecisionCountersSnapshot,
		routingPipeline.DecisionDurationHistogramSnapshot,
		routingPipeline.DecisionDurationSumSnapshot,
		routingPipeline.DecisionDurationCountSnapshot,
	)
	go func() {
		if err := adminSrv.Run(ctx); err != nil {
			logger.Warn("admin server exited", zap.Error(err))
		}
	}()

	// -----------------------------------------------------------------------
	// M3: Start port listeners
	// -----------------------------------------------------------------------

	var proxies []*proxy.Proxy

	for portID := range cfg.ServerConfig.Ports {
		portLogger := logger.With(zap.String("port", portID))
		srv, err := proxy.Initialize(ctx, &cfg, portLogger, topo, routingPipeline, portID)
		if err != nil {
			logger.Panic("failed to initialize proxy", zap.String("port", portID), zap.Error(err))
		}
		proxies = append(proxies, srv)

		go func(p *proxy.Proxy) {
			if err := p.Run(); err != nil {
				logger.Panic("server error", zap.String("port", p.Port()), zap.Error(err))
			}
		}(srv)
	}

	logger.Info("M3 startup complete",
		zap.Int("ports", len(proxies)),
		zap.Int("nodes", len(topo.Nodes())),
		zap.Int("policies", len(policyEng.Instances())),
	)

	if *noConsole {
		<-ctx.Done()
	} else {
		if len(proxies) > 0 {
			console.Start(ctx, proxies[0], logCh.Channel)
		} else {
			console.Start(ctx, nil, logCh.Channel)
		}
	}

	logger.Info("shutting down")

	decisionsBus.Close()

	if err := adminSrv.Close(); err != nil {
		logger.Warn("admin shutdown error", zap.Error(err))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	for _, p := range proxies {
		if err := p.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown timed out, forcing exit", zap.String("port", p.Port()), zap.Error(err))
		}
	}
}

func bindEnvKeys() {
	serverKeys := []string{
		"server.admin.listen_addr",
		"server.admin.tls_enabled",
		"server.admin.tls_cert_file",
		"server.admin.tls_key_file",
		"server.admin.metrics.listen_addr",
		"server.admin.metrics.auth_required",
		"server.admin.metrics.tls_enabled",
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
		"server.ports.read.listen_addr",
		"server.ports.read.pool_mode",
		"server.ports.read.ssl_mode",
		"server.ports.read.ssl_cert",
		"server.ports.read.ssl_key",
		"server.ports.compat.listen_addr",
		"server.ports.compat.pool_mode",
		"server.ports.compat.ssl_mode",
		"server.ports.compat.ssl_cert",
		"server.ports.compat.ssl_key",
	}

	probeKeys := []string{
		"probe.interval_ms",
		"probe.liveness_failure_count",
		"probe.liveness_max_age_ms",
		"probe.role_max_age_ms",
		"probe.lag_max_age_ms",
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
