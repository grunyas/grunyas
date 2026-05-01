package admin

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/topology"
)

type Server struct {
	topo   *topology.Topology
	cfg    *config.Config
	logger *zap.Logger

	httpSrv     *http.Server
	tlsConfig   *tls.Config
	tokenHashes map[string]string
	metricsReg  *prometheus.Registry
	metricsSrv  *http.Server

	requestsTotal *prometheus.CounterVec
	requestDur    *prometheus.HistogramVec

	closeOnce sync.Once

	decisionsBus *decisions.Bus
	policyEng    *policy.Engine

	routingMetrics struct {
		decisionsTotal    func() int64
		decisionsLeased   func() int64
		decisionsRejected func() int64
		publishedTotal    func() int64
		eligibleReadSize  func() int64
		eligibleWriteSize func() int64
	}
}

func (s *Server) SetRoutingMetrics(
	decisionsTotal func() int64,
	decisionsLeased func() int64,
	decisionsRejected func() int64,
	publishedTotal func() int64,
	eligibleReadSize func() int64,
	eligibleWriteSize func() int64,
) {
	s.routingMetrics.decisionsTotal = decisionsTotal
	s.routingMetrics.decisionsLeased = decisionsLeased
	s.routingMetrics.decisionsRejected = decisionsRejected
	s.routingMetrics.publishedTotal = publishedTotal
	s.routingMetrics.eligibleReadSize = eligibleReadSize
	s.routingMetrics.eligibleWriteSize = eligibleWriteSize
}

func New(topo *topology.Topology, cfg *config.Config, log *zap.Logger, bus *decisions.Bus, polEng *policy.Engine) (*Server, error) {
	s := &Server{
		topo:         topo,
		cfg:          cfg,
		logger:       log.With(zap.String("component", "admin")),
		tokenHashes:  make(map[string]string),
		metricsReg:   prometheus.NewRegistry(),
		decisionsBus: bus,
		policyEng:    polEng,
	}

	for token, entry := range cfg.ServerConfig.AdminTokens.Tokens {
		if token == "" {
			continue
		}
		h := sha256.Sum256([]byte(token))
		s.tokenHashes[hex.EncodeToString(h[:])] = entry.Role
	}

	ac := cfg.ServerConfig.Admin
	if ac.TLSEnabled {
		cert, err := tls.LoadX509KeyPair(ac.TLSCertFile, ac.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load admin TLS key pair: %w", err)
		}
		s.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	s.metricsReg.MustRegister(newTopologyCollector(topo))
	s.metricsReg.MustRegister(collectors.NewBuildInfoCollector())

	s.metricsReg.MustRegister(newDecisionCollector(s))

	s.requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "grunyas_admin_requests_total",
		Help: "Total admin API requests.",
	}, []string{"path", "method", "status"})
	s.requestDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "grunyas_admin_request_duration_seconds",
		Help:    "Admin API request duration.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"path"})
	s.metricsReg.MustRegister(s.requestsTotal, s.requestDur)

	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	mux := chi.NewMux()
	mux.Use(s.requestMetricsMiddleware)

	mux.Get("/healthz", s.handleHealthz)

	authRequired := s.cfg.ServerConfig.Admin.Metrics.AuthRequired
	if !authRequired {
		mux.Get("/metrics", s.handleMetrics)
	}

	mux.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		if authRequired {
			r.Get("/metrics", s.handleMetrics)
		}
		r.Get("/state", s.handleState)
		r.Get("/nodes", s.handleNodesList)
		r.Get("/nodes/{id}", s.handleNodeByID)
		r.Get("/pools", s.handlePools)
		r.Get("/config", s.handleConfig)
		r.Get("/policies", s.handlePolicies)
		r.Get("/policies/{name}", s.handlePolicyByName)
		r.Get("/decisions", s.handleDecisionsSSE)
	})

	addr := s.cfg.ServerConfig.Admin.ListenAddr
	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	mc := s.cfg.ServerConfig.Admin.Metrics
	if mc.ListenAddr != "" {
		if mc.ListenAddr == addr {
			return fmt.Errorf("server.admin.metrics.listen_addr %q must differ from server.admin.listen_addr", addr)
		}
		metricsAddr := mc.ListenAddr
		metricsMux := chi.NewMux()
		if s.cfg.ServerConfig.Admin.Metrics.AuthRequired {
			metricsMux.Use(s.authMiddleware)
		}
		metricsMux.Get("/metrics", s.handleMetrics)
		metricsMux.Get("/healthz", s.handleHealthz)

		s.metricsSrv = &http.Server{
			Addr:    metricsAddr,
			Handler: metricsMux,
		}
		go func() {
			s.logger.Info("dedicated metrics listener starting", zap.String("addr", metricsAddr))
			serveFn := s.metricsSrv.ListenAndServe
			if s.metricsTLSEnabled() {
				serveFn = func() error {
					return s.metricsSrv.ListenAndServeTLS(
						s.cfg.ServerConfig.Admin.TLSCertFile,
						s.cfg.ServerConfig.Admin.TLSKeyFile,
					)
				}
			}
			if err := serveFn(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Warn("metrics listener error", zap.Error(err))
			}
		}()
	}

	s.logger.Info("admin listener starting", zap.String("addr", addr))
	var lnErr error
	if s.tlsConfig != nil {
		ln, err := tls.Listen("tcp", addr, s.tlsConfig)
		if err != nil {
			return fmt.Errorf("admin TLS listen: %w", err)
		}
		lnErr = s.httpSrv.Serve(ln)
	} else {
		lnErr = s.httpSrv.ListenAndServe()
	}
	if lnErr != nil && !errors.Is(lnErr, http.ErrServerClosed) {
		return lnErr
	}
	return nil
}

// metricsTLSEnabled returns true when the metrics TLS config should be honored,
// falling back to the parent admin TLS config per M2 §2.
func (s *Server) metricsTLSEnabled() bool {
	mc := s.cfg.ServerConfig.Admin.Metrics
	if mc.TLSEnabled {
		return true
	}
	if s.cfg.ServerConfig.Admin.TLSEnabled && mc.ListenAddr != "" {
		return true
	}
	return false
}

func (s *Server) Close() error {
	var errs []string
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("admin: %v", err))
		}
		if s.metricsSrv != nil {
			if err := s.metricsSrv.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Sprintf("metrics: %v", err))
			}
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("admin shutdown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r)
		if token == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "missing authorization header")
			return
		}
		h := sha256.Sum256([]byte(token))
		role, ok := s.tokenHashes[hex.EncodeToString(h[:])]
		if !ok || role != "admin" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		dur := time.Since(start).Seconds()
		// Use chi route pattern so /nodes/{id} collapses into one series (M2 §7 #11).
		routePath := chi.RouteContext(r.Context()).RoutePattern()
		if routePath == "" {
			routePath = r.URL.Path
		}
		s.requestsTotal.WithLabelValues(routePath, r.Method, strconv.Itoa(ww.status)).Inc()
		s.requestDur.WithLabelValues(routePath).Observe(dur)
	})
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	nodes := s.topo.Nodes()
	primary := ""
	splitBrain := false

	clusterID := string(s.topo.ClusterID())

	p, ok := s.topo.Primary()
	if ok {
		primary = string(p.ID)
	} else {
		primaryCount := 0
		for _, nv := range nodes {
			if nv.ObservedRole == topology.RolePrimary {
				primaryCount++
			}
		}
		if primaryCount > 1 {
			splitBrain = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cluster": map[string]interface{}{
			"system_identifier": clusterID,
			"primary":           primary,
			"split_brain":       splitBrain,
		},
		"nodes":       s.nodeViewsToJSON(nodes),
		"pools":       s.poolViews(),
		"policies":    s.policyViews(),
		"observed_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleNodesList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"nodes": s.nodeViewsToJSON(s.topo.Nodes()),
	})
}

func (s *Server) handleNodeByID(w http.ResponseWriter, r *http.Request) {
	id := topology.NodeID(chi.URLParam(r, "id"))
	nv, ok := s.topo.Node(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not_found", fmt.Sprintf("node %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, s.nodeViewToJSON(nv))
}

func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pools": s.poolViews(),
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg

	// Marshal the full config through json tags so field names follow TOML snake_case
	// (M2 §3: "Field names match the TOML keys verbatim (snake_case)").
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to marshal config")
		return
	}

	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "failed to unmarshal config")
		return
	}

	// Post-process: redact passwords, strip token values, add token_count.
	redactConfig(m, s.tokenHashes)

	writeJSON(w, http.StatusOK, m)
}

// redactConfig walks a deserialized config map and redacts secrets per M2 §3 rules.
func redactConfig(m map[string]interface{}, tokenHashes map[string]string) {
	// Redact nodes[*].connection.password
	if nodes, ok := m["nodes"].([]interface{}); ok {
		for _, n := range nodes {
			if nodeMap, ok := n.(map[string]interface{}); ok {
				if conn, ok := nodeMap["connection"].(map[string]interface{}); ok {
					if pwStr, ok := conn["password"].(string); ok && pwStr != "" {
						conn["password"] = "***"
					}
				}
			}
		}
	}

	// Strip admin_tokens.tokens, expose token_count
	if server, ok := m["server"].(map[string]interface{}); ok {
		if adminTokens, ok := server["admin_tokens"].(map[string]interface{}); ok {
			delete(adminTokens, "tokens")
			adminTokens["token_count"] = len(tokenHashes)
		}
	}
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"policies": s.policyViews(),
	})
}

func (s *Server) handlePolicyByName(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if s.policyEng == nil {
		writeJSONError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("policy %q not found", name))
		return
	}
	for _, inst := range s.policyEng.Instances() {
		if inst.Name == name {
			writeJSON(w, http.StatusOK, s.policyView(inst))
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, "not_found",
		fmt.Sprintf("policy %q not found", name))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(s.metricsReg, promhttp.HandlerOpts{
		ErrorLog:      zap.NewStdLog(s.logger),
		ErrorHandling: promhttp.ContinueOnError,
	}).ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// Decisions SSE endpoint (M3)
// ---------------------------------------------------------------------------

func (s *Server) handleDecisionsSSE(w http.ResponseWriter, r *http.Request) {
	if s.decisionsBus == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "decisions bus not configured")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "response writer does not support flushing")
		return
	}

	sub, ok := s.decisionsBus.Subscribe()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "max_subscribers",
			"limit": s.cfg.ServerConfig.Decisions.MaxSubscribers,
		})
		return
	}
	defer sub.Unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case msg, ok := <-sub.Ch:
			if !ok {
				return
			}
			switch v := msg.(type) {
			case decisions.Event:
				data, err := json.Marshal(v)
				if err != nil {
					s.logger.Error("failed to marshal decision event for SSE", zap.Error(err))
					continue
				}
				_, _ = fmt.Fprintf(w, "event: decision\nid: %s\ndata: %s\n\n", v.EventID, data)
				flusher.Flush()
			case policy.Transition:
				data, err := json.Marshal(v)
				if err != nil {
					s.logger.Error("failed to marshal policy transition for SSE", zap.Error(err))
					continue
				}
				_, _ = fmt.Fprintf(w, "event: policy_transition\ndata: %s\n\n", data)
				flusher.Flush()
			}

			if drops := sub.DrainDropped(); drops > 0 {
				de := decisions.DroppedEvent{Count: drops, Since: time.Now().UTC().Format(time.RFC3339Nano)}
				data, err := json.Marshal(de)
				if err != nil {
					s.logger.Error("failed to marshal dropped event for SSE", zap.Error(err))
				} else {
					_, _ = fmt.Fprintf(w, "event: dropped\ndata: %s\n\n", data)
					flusher.Flush()
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Policy views (M3)
// ---------------------------------------------------------------------------

func (s *Server) policyViews() []interface{} {
	if s.policyEng == nil {
		return []interface{}{}
	}
	instances := s.policyEng.Instances()
	result := make([]interface{}, 0, len(instances))
	for _, inst := range instances {
		result = append(result, s.policyView(inst))
	}
	return result
}

func (s *Server) policyView(inst policy.Instance) map[string]interface{} {
	candidates := []map[string]interface{}{}
	states := s.policyEng.CandidateStates(inst.Name)
	for nodeID, cs := range states {
		entry := map[string]interface{}{
			"node_id":           nodeID,
			"state":             cs.State.String(),
			"entered_state_at":  cs.EnteredStateAt.UTC().Format(time.RFC3339Nano),
		}
		if cs.LastCondition != "" {
			entry["last_observation_reason"] = cs.LastCondition
		}
		candidates = append(candidates, entry)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i]["node_id"].(string) < candidates[j]["node_id"].(string)
	})

	return map[string]interface{}{
		"name":       inst.Name,
		"template":   inst.Template,
		"scope":      inst.Scope,
		"parameters": inst.Parameters,
		"timing": map[string]int{
			"dwell_ms":   inst.Timing.DwellMs,
			"release_ms": inst.Timing.ReleaseMs,
		},
		"candidates": candidates,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Server) poolViews() []map[string]interface{} {
	result := make([]map[string]interface{}, 0)
	nodes := s.topo.Nodes()

	for portID := range s.cfg.ServerConfig.Ports {
		portCfg := s.cfg.ServerConfig.Ports[portID]
		poolMode := portCfg.PoolMode
		if poolMode == "" {
			poolMode = "session"
		}

		for _, nv := range nodes {
			mgr, err := s.topo.PoolFor(nv.ID)
			if err != nil {
				continue
			}
			stats := mgr.PoolStats()

			minConns := 0
			for _, nc := range s.cfg.Nodes {
				if nc.ID == string(nv.ID) {
					minConns = nc.Pool.MinConns
					break
				}
			}

			result = append(result, map[string]interface{}{
				"port":           portID,
				"node_id":        string(nv.ID),
				"mode":           poolMode,
				"total_conns":    stats.TotalConns,
				"acquired_conns": stats.AcquiredConns,
				"idle_conns":     stats.IdleConns,
				"max_conns":      stats.MaxConns,
				"min_conns":      minConns,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		pi := result[i]["port"].(string)
		pj := result[j]["port"].(string)
		if pi != pj {
			return pi < pj
		}
		return result[i]["node_id"].(string) < result[j]["node_id"].(string)
	})

	return result
}

func (s *Server) nodeViewToJSON(nv topology.NodeView) map[string]interface{} {
	clusterID := s.topo.ClusterID()
	j := map[string]interface{}{
		"id":                      string(nv.ID),
		"host":                    nv.Host,
		"port":                    nv.Port,
		"declared_role":           nv.DeclaredRole.String(),
		"observed_role":           nv.ObservedRole.String(),
		"role_disagreement":       nv.ObservedRole != topology.RoleUnknown && nv.ObservedRole != nv.DeclaredRole,
		"liveness":                nv.Liveness.String(),
		"liveness_state":          livenessState(nv),
		"system_identifier":       string(nv.SystemID),
		"system_identifier_match": nv.SystemID != "" && nv.SystemID == clusterID,
		"replication_lag_state":   nv.ReplicationLagState.String(),
		"last_probe_at":           nv.LastProbeAt.UTC().Format(time.RFC3339Nano),
		"last_lag_sample_at":      nv.LastLagSampleAt.UTC().Format(time.RFC3339Nano),
	}
	if nv.ReplicationLagMs != nil {
		j["replication_lag_ms"] = *nv.ReplicationLagMs
	} else {
		j["replication_lag_ms"] = nil
	}
	if nv.LastProbeErr != nil {
		j["last_probe_error"] = nv.LastProbeErr.Error()
	} else {
		j["last_probe_error"] = nil
	}
	return j
}

// livenessState computes the freshness of the liveness observation
// per M2 §1 stale-observation semantics.
func livenessState(nv topology.NodeView) string {
	if nv.LastProbeAt.IsZero() {
		return "unknown"
	}
	if time.Since(nv.LastProbeAt).Milliseconds() > int64(nv.LivenessMaxAgeMs) {
		return "stale"
	}
	return "fresh"
}

func (s *Server) nodeViewsToJSON(nvs []topology.NodeView) []map[string]interface{} {
	result := make([]map[string]interface{}, len(nvs))
	for i, nv := range nvs {
		result[i] = s.nodeViewToJSON(nv)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i]["id"].(string) < result[j]["id"].(string)
	})
	return result
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------------------
// Topology Prometheus collector
// ---------------------------------------------------------------------------

type topologyCollector struct {
	topo *topology.Topology
}

func newTopologyCollector(topo *topology.Topology) *topologyCollector {
	return &topologyCollector{topo: topo}
}

var (
	buildInfoDesc   = prometheus.NewDesc("grunyas_build_info", "Build info", []string{"version", "commit", "go_version"}, nil)
	nodesTotalDesc  = prometheus.NewDesc("grunyas_nodes_total", "Total nodes", nil, nil)
	nodesByRoleDesc = prometheus.NewDesc("grunyas_nodes_by_role", "Nodes by role", []string{"role"}, nil)
	nodesByLiveDesc = prometheus.NewDesc("grunyas_nodes_by_liveness", "Nodes by liveness", []string{"liveness"}, nil)
	nodeLiveDesc    = prometheus.NewDesc("grunyas_node_liveness", "Node liveness", []string{"node_id"}, nil)
	nodeRoleDesc    = prometheus.NewDesc("grunyas_node_observed_role", "Node observed role", []string{"node_id", "role"}, nil)
	nodeDisagDesc   = prometheus.NewDesc("grunyas_node_role_disagreement", "Node role disagreement", []string{"node_id"}, nil)
	nodeLagDesc     = prometheus.NewDesc("grunyas_node_replication_lag_ms", "Node replication lag ms", []string{"node_id"}, nil)
	nodeObsAgeDesc  = prometheus.NewDesc("grunyas_node_observation_age_seconds", "Observation age", []string{"node_id", "property"}, nil)
)

func (c *topologyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- buildInfoDesc
	ch <- nodesTotalDesc
	ch <- nodesByRoleDesc
	ch <- nodesByLiveDesc
	ch <- nodeLiveDesc
	ch <- nodeRoleDesc
	ch <- nodeDisagDesc
	ch <- nodeLagDesc
	ch <- nodeObsAgeDesc
}

func (c *topologyCollector) Collect(ch chan<- prometheus.Metric) {
	bi, ok := debug.ReadBuildInfo()
	if ok {
		version := "unknown"
		commit := "unknown"
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				commit = s.Value
			}
		}
		ch <- prometheus.MustNewConstMetric(buildInfoDesc, prometheus.GaugeValue, 1, version, commit, bi.GoVersion)
	}

	nodes := c.topo.Nodes()
	now := time.Now()

	ch <- prometheus.MustNewConstMetric(nodesTotalDesc, prometheus.GaugeValue, float64(len(nodes)))

	byRole := map[string]float64{"primary": 0, "replica": 0, "unknown": 0}
	byLive := map[string]float64{"up": 0, "degraded": 0, "down": 0, "unknown": 0}

	for _, nv := range nodes {
		byRole[nv.ObservedRole.String()]++
		byLive[nv.Liveness.String()]++

		livenessVal := math.NaN()
		switch nv.Liveness {
		case topology.LivenessUp:
			livenessVal = 1
		case topology.LivenessDegraded:
			livenessVal = 0.5
		case topology.LivenessDown:
			livenessVal = 0
		}
		ch <- prometheus.MustNewConstMetric(nodeLiveDesc, prometheus.GaugeValue, livenessVal, string(nv.ID))

		for _, role := range []string{"primary", "replica", "unknown"} {
			v := 0.0
			if nv.ObservedRole.String() == role {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(nodeRoleDesc, prometheus.GaugeValue, v, string(nv.ID), role)
		}

		disagreement := 0.0
		if nv.ObservedRole != topology.RoleUnknown && nv.ObservedRole != nv.DeclaredRole {
			disagreement = 1
		}
		ch <- prometheus.MustNewConstMetric(nodeDisagDesc, prometheus.GaugeValue, disagreement, string(nv.ID))

		if nv.ReplicationLagMs != nil && nv.ReplicationLagState == topology.LagStateFresh {
			ch <- prometheus.MustNewConstMetric(nodeLagDesc, prometheus.GaugeValue, float64(*nv.ReplicationLagMs), string(nv.ID))
		}

		if !nv.LastProbeAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(nodeObsAgeDesc, prometheus.GaugeValue, now.Sub(nv.LastProbeAt).Seconds(), string(nv.ID), "liveness")
		}
		if !nv.LastLagSampleAt.IsZero() {
			ch <- prometheus.MustNewConstMetric(nodeObsAgeDesc, prometheus.GaugeValue, now.Sub(nv.LastLagSampleAt).Seconds(), string(nv.ID), "lag")
		}
	}

	for role, count := range byRole {
		ch <- prometheus.MustNewConstMetric(nodesByRoleDesc, prometheus.GaugeValue, count, role)
	}
	for l, count := range byLive {
		ch <- prometheus.MustNewConstMetric(nodesByLiveDesc, prometheus.GaugeValue, count, l)
	}
}

// ---------------------------------------------------------------------------
// M3: Decision + Policy + Bus Prometheus collector
// ---------------------------------------------------------------------------

type decisionCollector struct {
	srv *Server
}

func newDecisionCollector(srv *Server) *decisionCollector {
	return &decisionCollector{srv: srv}
}

var (
	decTotalDesc         = prometheus.NewDesc("grunyas_routing_decisions_total", "Total routing decisions.", []string{"port", "outcome", "reason"}, nil)
	decEligSetSizeDesc   = prometheus.NewDesc("grunyas_routing_eligible_set_size", "Eligible set size.", []string{"port"}, nil)
	decDurDesc           = prometheus.NewDesc("grunyas_routing_decision_duration_seconds", "Decision duration seconds.", []string{"port"}, nil)
	policyStateDesc      = prometheus.NewDesc("grunyas_policy_state", "Policy state per candidate.", []string{"policy", "scope", "node_id", "state"}, nil)
	policyTransDesc      = prometheus.NewDesc("grunyas_policy_transitions_total", "Policy transition count.", []string{"policy", "scope", "node_id", "from", "to"}, nil)
	subsGaugeDesc        = prometheus.NewDesc("grunyas_decisions_subscribers", "Active SSE subscribers.", nil, nil)
	subsPublishedDesc    = prometheus.NewDesc("grunyas_decisions_published_total", "Published decision events.", nil, nil)
	subsDroppedDesc      = prometheus.NewDesc("grunyas_decisions_dropped_total", "Dropped decision events.", []string{"reason"}, nil)
)

func (c *decisionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- decTotalDesc
	ch <- decEligSetSizeDesc
	ch <- decDurDesc
	ch <- policyStateDesc
	ch <- policyTransDesc
	ch <- subsGaugeDesc
	ch <- subsPublishedDesc
	ch <- subsDroppedDesc
}

func (c *decisionCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.srv

	// Routing decisions
	rm := s.routingMetrics
	if rm.decisionsTotal != nil {
		total := float64(rm.decisionsTotal())
		ch <- prometheus.MustNewConstMetric(decTotalDesc, prometheus.CounterValue, total, "all", "all", "all")

		if rm.decisionsLeased != nil {
			ch <- prometheus.MustNewConstMetric(decTotalDesc, prometheus.CounterValue, float64(rm.decisionsLeased()), "all", "leased", "")
		}
		if rm.decisionsRejected != nil {
			ch <- prometheus.MustNewConstMetric(decTotalDesc, prometheus.CounterValue, float64(rm.decisionsRejected()), "all", "rejected", "")
		}
	}
	if rm.eligibleReadSize != nil {
		ch <- prometheus.MustNewConstMetric(decEligSetSizeDesc, prometheus.GaugeValue, float64(rm.eligibleReadSize()), "read")
	}
	if rm.eligibleWriteSize != nil {
		ch <- prometheus.MustNewConstMetric(decEligSetSizeDesc, prometheus.GaugeValue, float64(rm.eligibleWriteSize()), "write")
	}

	// Policy states
	if s.policyEng != nil {
		for _, inst := range s.policyEng.Instances() {
			for nodeID, cs := range s.policyEng.CandidateStates(inst.Name) {
				ch <- prometheus.MustNewConstMetric(policyStateDesc, prometheus.GaugeValue, 1, inst.Name, inst.Scope, nodeID, cs.State.String())
			}
		}
	}

	// Bus metrics
	if s.decisionsBus != nil {
		ch <- prometheus.MustNewConstMetric(subsGaugeDesc, prometheus.GaugeValue, float64(s.decisionsBus.SubscriberCount()))
		if rm.publishedTotal != nil {
			ch <- prometheus.MustNewConstMetric(subsPublishedDesc, prometheus.CounterValue, float64(rm.publishedTotal()))
		}
		ch <- prometheus.MustNewConstMetric(subsDroppedDesc, prometheus.CounterValue, float64(s.decisionsBus.DroppedOTelOverflow()), "otel_overflow")
		ch <- prometheus.MustNewConstMetric(subsDroppedDesc, prometheus.CounterValue, float64(s.decisionsBus.DroppedSubOverflow()), "subscriber_overflow")
		ch <- prometheus.MustNewConstMetric(subsDroppedDesc, prometheus.CounterValue, float64(s.decisionsBus.DroppedBusOverflow()), "bus_overflow")
	}
}
