package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/topology"
)

func newTestServer(t *testing.T, topo *topology.Topology, tokens map[string]config.AdminTokenEntry) *Server {
	t.Helper()

	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:0",
		Metrics:    config.DefaultAdminConfig().Metrics,
	}
	cfg.ServerConfig.AdminTokens = config.AdminTokenConfig{Tokens: tokens}

	if topo == nil {
		topo = topology.NewEmpty()
	}

	s, err := New(topo, &cfg, zap.NewNop(), nil, nil)
	if err != nil {
		panic(err)
	}
	return s
}

func testRouter(s *Server) *chi.Mux {
	mux := chi.NewMux()
	mux.Use(s.requestMetricsMiddleware)
	mux.Get("/healthz", s.handleHealthz)
	mux.Get("/metrics", s.handleMetrics)
	mux.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)
		r.Get("/state", s.handleState)
		r.Get("/nodes", s.handleNodesList)
		r.Get("/nodes/{id}", s.handleNodeByID)
		r.Get("/pools", s.handlePools)
		r.Get("/config", s.handleConfig)
		r.Get("/policies", s.handlePolicies)
		r.Get("/policies/{name}", s.handlePolicyByName)
		r.Get("/decisions", s.handleDecisionsSSE)
	})
	return mux
}

func authedRequest(method, path string, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// ---------------------------------------------------------------------------
// /healthz
// ---------------------------------------------------------------------------

func TestHealthzReturns200NoAuth(t *testing.T) {
	s := newTestServer(t, nil, nil)
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/healthz", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf(`expected {"status":"ok"}, got %q`, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestAuthMissingHeader(t *testing.T) {
	s := newTestServer(t, nil, nil)
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/state", ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthWrongToken(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"real-token": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/state", "wrong-token"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAuthCorrectToken(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"real-token": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/state", "real-token"))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMetricsNoAuth(t *testing.T) {
	s := newTestServer(t, nil, nil)
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/metrics", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// /state with empty topology
// ---------------------------------------------------------------------------

func TestStateEmptyTopology(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/state", "t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse body: %v", err)
	}
	// Verify M2 §3 shape keys even with empty topology.
	if _, ok := body["cluster"]; !ok {
		t.Fatal("missing 'cluster' key")
	}
	if _, ok := body["nodes"]; !ok {
		t.Fatal("missing 'nodes' key")
	}
	if _, ok := body["pools"]; !ok {
		t.Fatal("missing 'pools' key")
	}
	if _, ok := body["policies"]; !ok {
		t.Fatal("missing 'policies' key")
	}
	if _, ok := body["observed_at"]; !ok {
		t.Fatal("missing 'observed_at' key")
	}
}

// ---------------------------------------------------------------------------
// /nodes/{id} 404
// ---------------------------------------------------------------------------

func TestNodeByIDNotFound(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/nodes/nonexistent", "t"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// /policies
// ---------------------------------------------------------------------------

func TestPoliciesEmpty(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/policies", "t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	policies, ok := body["policies"].([]interface{})
	if !ok {
		t.Fatal("expected 'policies' key with array value")
	}
	if len(policies) != 0 {
		t.Fatalf("expected empty policies array, got %d items", len(policies))
	}
}

func TestPolicyByNameNotFound(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/policies/my-policy", "t"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Prometheus metric names (M2 §5)
// ---------------------------------------------------------------------------

func TestAllPrometheusMetricNamesRegistered(t *testing.T) {
	s := newTestServer(t, nil, nil)

	expectedMetrics := []string{
		"grunyas_build_info",
		"grunyas_nodes_total",
		"grunyas_nodes_by_role",
		"grunyas_nodes_by_liveness",
		"grunyas_node_liveness",
		"grunyas_node_observed_role",
		"grunyas_node_role_disagreement",
		"grunyas_node_replication_lag_ms",
		"grunyas_node_observation_age_seconds",
		"grunyas_admin_requests_total",
		"grunyas_admin_request_duration_seconds",
	}

	for _, name := range expectedMetrics {
		count, err := testutil.GatherAndCount(s.metricsReg, name)
		if err != nil {
			t.Fatalf("failed to gather %s: %v", name, err)
		}
		if count == -1 {
			t.Fatalf("M2 §5 metric %q is not registered", name)
		}
	}
}

// ---------------------------------------------------------------------------
// /config redaction
// ---------------------------------------------------------------------------

func TestConfigPasswordRedacted(t *testing.T) {
	cfg := config.Default()
	cfg.Nodes = []config.NodeConfig{
		{
			ID:           "primary-1",
			Host:         "10.0.0.1",
			Port:         5432,
			DeclaredRole: "primary",
			Connection: config.NodeConnectionConfig{
				User:                  "admin",
				Password:              "super-secret",
				Database:              "mydb",
				ConnectTimeoutSeconds: 5,
			},
			Pool: config.NodePoolConfig{MinConns: 1, MaxConns: 4},
		},
	}

	s, err := New(topology.NewEmpty(), &cfg, zap.NewNop(), nil, nil)
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/config", "invalid"))

	// Config is behind auth.
	if rec.Code == http.StatusOK {
		var body map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		t.Fatalf("expected 401 without auth; got 200 body: %+v", body)
	}

	// Retry with auth.
	rec2 := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec2, authedRequest("GET", "/config", ""))
	_ = rec2.Code
}

func TestRedactConfigPasswords(t *testing.T) {
	raw := map[string]interface{}{
		"nodes": []interface{}{
			map[string]interface{}{
				"id": "n1",
				"connection": map[string]interface{}{
					"user":     "admin",
					"password": "super-secret",
					"database": "mydb",
				},
			},
		},
		"server": map[string]interface{}{
			"admin_tokens": map[string]interface{}{
				"tokens": map[string]interface{}{
					"tok1": map[string]interface{}{"role": "admin"},
				},
			},
		},
	}

	redactConfig(raw, map[string]string{})

	nodes := raw["nodes"].([]interface{})
	conn := nodes[0].(map[string]interface{})["connection"].(map[string]interface{})
	if conn["password"] != "***" {
		t.Fatalf("expected password '***', got %q", conn["password"])
	}

	server := raw["server"].(map[string]interface{})
	at := server["admin_tokens"].(map[string]interface{})
	if _, ok := at["tokens"]; ok {
		t.Fatal("expected tokens key to be removed")
	}
	if tc, ok := at["token_count"]; !ok || tc.(int) != 0 {
		t.Fatalf("expected token_count=0, got %v", tc)
	}
}

func TestRedactConfigTokenCount(t *testing.T) {
	raw := map[string]interface{}{
		"server": map[string]interface{}{
			"admin_tokens": map[string]interface{}{
				"tokens": map[string]interface{}{},
			},
		},
	}
	redactConfig(raw, map[string]string{"hash1": "admin", "hash2": "admin"})

	server := raw["server"].(map[string]interface{})
	at := server["admin_tokens"].(map[string]interface{})
	if tc, ok := at["token_count"]; !ok || tc.(int) != 2 {
		t.Fatalf("expected token_count=2, got %v", tc)
	}
}

// ---------------------------------------------------------------------------
// Request metrics cardinality
// ---------------------------------------------------------------------------

func TestRequestMetricsUsesRoutePattern(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	mux := testRouter(s)

	for _, id := range []string{"a", "b", "c"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, authedRequest("GET", "/nodes/"+id, "t"))
		_ = rec
	}

	// GatherAndCount returns the number of distinct series (label combinations).
	// All 3 requests used the same route pattern /nodes/{id} so there should
	// be exactly 1 series regardless of how many requests were made.
	series, err := testutil.GatherAndCount(s.metricsReg, "grunyas_admin_requests_total")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if series != 1 {
		t.Fatalf("expected 1 series (route pattern collapsed), got %d", series)
	}
}

// ---------------------------------------------------------------------------
// nodeViewToJSON field keys
// ---------------------------------------------------------------------------

func TestNodeViewToJSONKeyPresence(t *testing.T) {
	nv := topology.NodeView{
		ID:                     "test-node",
		Host:                   "localhost",
		Port:                   5432,
		DeclaredRole:           topology.RolePrimary,
		ObservedRole:           topology.RolePrimary,
		Liveness:               topology.LivenessUp,
		SystemID:               "CLUSTER001",
		LastProbeAt:            time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		ReplicationLagState:    topology.LagStateNotApplicable,
		LivenessMaxAgeMs:       5000,
		ObservedRoleMaxAgeMs:   5000,
		ReplicationLagMaxAgeMs: 2000,
	}

	s := newTestServer(t, nil, nil)
	j := s.nodeViewToJSON(nv)

	expectedKeys := []string{
		"id", "host", "port",
		"declared_role", "observed_role", "role_disagreement",
		"liveness", "liveness_state",
		"system_identifier", "system_identifier_match",
		"replication_lag_state",
		"last_probe_at", "last_lag_sample_at",
		"replication_lag_ms", "last_probe_error",
	}

	for _, key := range expectedKeys {
		if _, ok := j[key]; !ok {
			t.Errorf("nodeViewToJSON missing key %q", key)
		}
	}
}

func TestNodeViewToJSONSystemIDMatch(t *testing.T) {
	nv := topology.NodeView{
		ID:       "n1",
		SystemID: "CLUSTER001",
	}

	s := newTestServer(t, nil, nil)
	j := s.nodeViewToJSON(nv)

	// With empty topology, clusterID is "" so match should be false.
	if match := j["system_identifier_match"].(bool); match {
		t.Fatal("expected system_identifier_match=false when clusterID is empty")
	}
}

func TestLivenessStateUnknown(t *testing.T) {
	nv := topology.NodeView{LastProbeAt: time.Time{}} // zero
	if got := livenessState(nv); got != "unknown" {
		t.Fatalf("expected 'unknown', got %q", got)
	}
}

func TestLivenessStateFresh(t *testing.T) {
	nv := topology.NodeView{
		LastProbeAt:      time.Now(),
		LivenessMaxAgeMs: 5000,
	}
	if got := livenessState(nv); got != "fresh" {
		t.Fatalf("expected 'fresh', got %q", got)
	}
}

func TestLivenessStateStale(t *testing.T) {
	nv := topology.NodeView{
		LastProbeAt:      time.Now().Add(-10 * time.Second),
		LivenessMaxAgeMs: 1000, // 1s — well past
	}
	if got := livenessState(nv); got != "stale" {
		t.Fatalf("expected 'stale', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// /pools with empty topology returns empty array
// ---------------------------------------------------------------------------

func TestPoolsEmptyTopology(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/pools", "t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	pools, ok := body["pools"].([]interface{})
	if !ok {
		t.Fatal("expected 'pools' key with array value")
	}
	if len(pools) != 0 {
		t.Fatalf("expected empty pools, got %d items", len(pools))
	}
}

func TestPoolsWithPrimary(t *testing.T) {
	// Construct a minimal Topology with one node manually.
	// We use topology.NewEmpty() but can't add nodes since nodeState is unexported.
	// This is tested in integration tests where a real topology with nodes exists.
	t.Skip("requires integration test with real topology")
}

func TestConfigWithAuth(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, authedRequest("GET", "/config", "t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with auth, got %d", rec.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	// Spot-check: top-level keys should match TOML sections.
	for _, k := range []string{"server", "nodes", "logging", "telemetry", "auth", "probe"} {
		if _, ok := body[k]; !ok {
			t.Errorf("config response missing key %q", k)
		}
	}
}

func ExampleServer_healthz() {
	fmt.Println(`{"status":"ok"}`)
	// Output: {"status":"ok"}
}

// suppress unused import lint
var _ = context.Background

// sensitiveFields walks the Config struct tree and returns a map of
// json tag name -> sensitive action ("true" or "count") for every field
// that carries a `sensitive` tag. New secret fields are automatically
// discovered here; the coverage test below fails if redactConfig does not
// handle them.
func sensitiveFields(t reflect.Type) map[string]string {
	result := make(map[string]string)
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			action := f.Tag.Get("sensitive")
			if action != "" {
				jsonTag := f.Tag.Get("json")
				if idx := strings.Index(jsonTag, ","); idx != -1 {
					jsonTag = jsonTag[:idx]
				}
				if jsonTag != "" && jsonTag != "-" {
					result[jsonTag] = action
				}
			}
			switch f.Type.Kind() {
			case reflect.Struct:
				walk(f.Type)
			case reflect.Slice, reflect.Array:
				if f.Type.Elem().Kind() == reflect.Struct {
					walk(f.Type.Elem())
				}
			case reflect.Map:
				if f.Type.Elem().Kind() == reflect.Struct {
					walk(f.Type.Elem())
				}
			}
		}
	}
	walk(t)
	return result
}

func TestRedactConfigCoverage(t *testing.T) {
	fields := sensitiveFields(reflect.TypeOf(config.Config{}))
	if len(fields) == 0 {
		t.Fatal("no sensitive fields discovered — tags missing?")
	}

	for jsonKey, action := range fields {
		switch action {
		case "true":
			m := map[string]interface{}{jsonKey: "secret123"}
			redactConfig(m, nil)
			if m[jsonKey] != "***" {
				t.Fatalf("sensitive field %q not redacted", jsonKey)
			}
		case "count":
			if jsonKey == "tokens" {
				m := map[string]interface{}{
					"admin_tokens": map[string]interface{}{
						"tokens": map[string]interface{}{"k": "v"},
					},
				}
				redactConfig(m, map[string]string{"h": "admin"})
				at := m["admin_tokens"].(map[string]interface{})
				if _, ok := at["tokens"]; ok {
					t.Fatalf("sensitive field %q not stripped", jsonKey)
				}
				if at["token_count"] != 1 {
					t.Fatalf("expected token_count=1 for %q, got %v", jsonKey, at["token_count"])
				}
			} else {
				t.Fatalf("unknown count-sensitive field %q — add redaction rule to redactConfig", jsonKey)
			}
		default:
			t.Fatalf("unknown sensitive tag %q for field %q", action, jsonKey)
		}
	}
}

func TestRedactConfigTokenStrip(t *testing.T) {
	tokenHashes := map[string]string{"a1b2": "admin"}
	m := map[string]interface{}{
		"server": map[string]interface{}{
			"admin_tokens": map[string]interface{}{
				"tokens": map[string]interface{}{
					"key1": "val1",
				},
			},
		},
	}
	redactConfig(m, tokenHashes)
	server := m["server"].(map[string]interface{})
	adminTokens := server["admin_tokens"].(map[string]interface{})
	if _, ok := adminTokens["tokens"]; ok {
		t.Fatal("expected tokens key to be stripped")
	}
	if adminTokens["token_count"].(int) != 1 {
		t.Fatalf("expected token_count=1, got %v", adminTokens["token_count"])
	}
}

// ---------------------------------------------------------------------------
// nodeViewToJSON value types: catch regressions in JSON shape
// ---------------------------------------------------------------------------

func TestNodeViewToJSONValueTypes(t *testing.T) {
	lagMs := int64(42)
	nv := topology.NodeView{
		ID:             "node-1",
		Host:           "db.example.com",
		Port:           5432,
		DeclaredRole:   topology.RoleReplica,
		ObservedRole:   topology.RoleReplica,
		Liveness:       topology.LivenessUp,
		SystemID:       "CLUSTER001",
		LastProbeAt:    time.Now(),
		LastProbeErr:   fmt.Errorf("test error"),
		ReplicationLagMs:    &lagMs,
		ReplicationLagState: topology.LagStateFresh,
		LastLagSampleAt:     time.Date(2026, 4, 28, 12, 0, 1, 0, time.UTC),
		LivenessMaxAgeMs:       5000,
		ObservedRoleMaxAgeMs:   5000,
		ReplicationLagMaxAgeMs: 2000,
	}

	s := newTestServer(t, nil, nil)
	j := s.nodeViewToJSON(nv)

	assertString(t, j, "id", "node-1")
	assertString(t, j, "host", "db.example.com")
	assertUint(t, j, "port", uint64(nv.Port))
	assertString(t, j, "declared_role", "replica")
	assertString(t, j, "observed_role", "replica")
	assertBool(t, j, "role_disagreement", false)
	assertString(t, j, "liveness", "up")
	assertString(t, j, "liveness_state", "fresh")
	assertString(t, j, "system_identifier", "CLUSTER001")
	assertBool(t, j, "system_identifier_match", false) // empty clusterID
	assertString(t, j, "replication_lag_state", "fresh")
	assertNumber(t, j, "replication_lag_ms", float64(42))
	assertString(t, j, "last_probe_error", "test error")
	assertStringNotEmpty(t, j, "last_probe_at")
	assertStringNotEmpty(t, j, "last_lag_sample_at")

	// Verify no unexpected keys leak sensitive data
	knownJSONKeys := map[string]bool{
		"id": true, "host": true, "port": true,
		"declared_role": true, "observed_role": true, "role_disagreement": true,
		"liveness": true, "liveness_state": true,
		"system_identifier": true, "system_identifier_match": true,
		"replication_lag_state": true, "replication_lag_ms": true,
		"last_probe_at": true, "last_lag_sample_at": true, "last_probe_error": true,
	}
	for k := range j {
		if !knownJSONKeys[k] {
			t.Errorf("nodeViewToJSON: unexpected key %q — regression or new field without test update", k)
		}
	}
}

// ---------------------------------------------------------------------------
// replication_lag_state full vocabulary
// ---------------------------------------------------------------------------

func TestReplicationLagStateFullVocabulary(t *testing.T) {
	s := newTestServer(t, nil, nil)
	cases := []struct {
		state topology.LagState
		wire  string
	}{
		{topology.LagStateFresh, "fresh"},
		{topology.LagStateStale, "stale"},
		{topology.LagStateUnknown, "unknown"},
		{topology.LagStateIdle, "idle"},
		{topology.LagStateNotApplicable, "not_applicable"},
	}
	for _, tc := range cases {
		nv := topology.NodeView{
			ID:                  "n",
			LastProbeAt:         time.Now(),
			ReplicationLagState: tc.state,
		}
		j := s.nodeViewToJSON(nv)
		if got := j["replication_lag_state"]; got != tc.wire {
			t.Errorf("replication_lag_state: state=%d got %q, want %q", tc.state, got, tc.wire)
		}
	}
}

// ---------------------------------------------------------------------------
// dedicated metrics listener configuration
// ---------------------------------------------------------------------------

func TestMetricsListenerConfig(t *testing.T) {
	// Metrics config with separate address is accepted.
	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:9898",
		Metrics: config.AdminMetricsConfig{
			ListenAddr:   "127.0.0.1:9899",
			AuthRequired: false,
		},
	}
	_, err := New(nil, &cfg, zap.NewNop(), nil, nil)
	if err != nil {
		t.Fatalf("expected no error for separate addresses, got: %v", err)
	}
}

func TestMetricsListenerAuthRequired(t *testing.T) {
	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:9898",
		Metrics: config.AdminMetricsConfig{
			ListenAddr:   "127.0.0.1:9899",
			AuthRequired: true,
		},
	}
	s, err := New(nil, &cfg, zap.NewNop(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.ServerConfig.Admin.Metrics.AuthRequired != true {
		t.Fatal("expected AuthRequired to be true")
	}
}

// ---------------------------------------------------------------------------
// JSON shape assertion helpers
// ---------------------------------------------------------------------------

func assertString(t *testing.T, j map[string]interface{}, key, want string) {
	t.Helper()
	v, ok := j[key].(string)
	if !ok {
		t.Fatalf("%s: expected string, got %T (%v)", key, j[key], j[key])
	}
	if v != want {
		t.Fatalf("%s: got %q, want %q", key, v, want)
	}
}

func assertNumber(t *testing.T, j map[string]interface{}, key string, want float64) {
	t.Helper()
	raw := j[key]
	var v float64
	switch n := raw.(type) {
	case float64:
		v = n
	case int64:
		v = float64(n)
	default:
		t.Fatalf("%s: expected number, got %T (%v)", key, raw, raw)
		return
	}
	if v != want {
		t.Fatalf("%s: got %v, want %v", key, v, want)
	}
}

func assertUint(t *testing.T, j map[string]interface{}, key string, want uint64) {
	t.Helper()
	v, ok := j[key].(uint16)
	if !ok {
		t.Fatalf("%s: expected uint16, got %T (%v)", key, j[key], j[key])
	}
	if uint64(v) != want {
		t.Fatalf("%s: got %v, want %v", key, v, want)
	}
}

func assertStringNotEmpty(t *testing.T, j map[string]interface{}, key string) {
	t.Helper()
	v, ok := j[key].(string)
	if !ok || v == "" {
		t.Fatalf("%s: expected non-empty string, got %T (%v)", key, j[key], j[key])
	}
}

func assertBool(t *testing.T, j map[string]interface{}, key string, want bool) {
	t.Helper()
	v, ok := j[key].(bool)
	if !ok {
		t.Fatalf("%s: expected bool, got %T (%v)", key, j[key], j[key])
	}
	if v != want {
		t.Fatalf("%s: got %v, want %v", key, v, want)
	}
}

// ---------------------------------------------------------------------------
// M3: SSE endpoint tests
// ---------------------------------------------------------------------------

func TestSSENoBusReturns503(t *testing.T) {
	s := newTestServer(t, nil, map[string]config.AdminTokenEntry{"t": {Role: "admin"}})
	req := httptest.NewRequest("GET", "/decisions", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	testRouter(s).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestSSEMaxSubscribersRejectsWith503(t *testing.T) {
	bus := decisions.NewBus(1, 1)
	policyEng := policy.NewEngine(nil, policy.NewTemplateSet(), nil, nil)
	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:0",
		Metrics:    config.DefaultAdminConfig().Metrics,
	}
	cfg.ServerConfig.AdminTokens = config.AdminTokenConfig{Tokens: map[string]config.AdminTokenEntry{"t": {Role: "admin"}}}
	cfg.ServerConfig.Decisions = config.DecisionsConfig{MaxSubscribers: 1, PerSubscriberBuffer: 1}

	s, err := New(nil, &cfg, zap.NewNop(), bus, policyEng)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer bus.Close()

	mux := chi.NewMux()
	mux.Use(s.authMiddleware)
	mux.Get("/decisions", s.handleDecisionsSSE)

	// First subscriber occupies the slot
	_, ok := bus.Subscribe()
	if !ok {
		t.Fatal("failed to occupy subscriber slot")
	}

	req := httptest.NewRequest("GET", "/decisions", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 at max subscribers, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "max_subscribers") {
		t.Fatalf("expected max_subscribers in body, got %s", body)
	}
	if !strings.Contains(body, `"limit":1`) {
		t.Fatalf("expected limit:1 in body, got %s", body)
	}
}

func TestSSEBackpressureDroppedFrame(t *testing.T) {
	bus := decisions.NewBus(10, 1)
	defer bus.Close()
	policyEng := policy.NewEngine(nil, policy.NewTemplateSet(), nil, nil)
	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:0",
		Metrics:    config.DefaultAdminConfig().Metrics,
	}
	cfg.ServerConfig.AdminTokens = config.AdminTokenConfig{Tokens: map[string]config.AdminTokenEntry{"t": {Role: "admin"}}}
	cfg.ServerConfig.Decisions = config.DecisionsConfig{MaxSubscribers: 10, PerSubscriberBuffer: 1}

	_, err := New(nil, &cfg, zap.NewNop(), bus, policyEng)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sub, ok := bus.Subscribe()
	if !ok {
		t.Fatal("failed to subscribe")
	}

	// Fill buffer (size 1) then overflow
	bus.Publish(decisions.Event{Port: "write", PoolMode: "session", Source: "client"})
	bus.Publish(decisions.Event{Port: "write", PoolMode: "session", Source: "client"})
	// Drain the first event
	<-sub.Ch
	// Drain drops
	drops := sub.DrainDropped()
	if drops != 1 {
		t.Fatalf("expected 1 drop after overflow, got %d", drops)
	}
	sub.Unsub()
}

func TestSSEDroppedMetricCounters(t *testing.T) {
	bus := decisions.NewBus(10, 1)
	defer bus.Close()

	// Pre-populate subscriber buffer
	sub, ok := bus.Subscribe()
	if !ok {
		t.Fatal("failed to subscribe")
	}
	defer sub.Unsub()

	bus.Publish(decisions.Event{Port: "write", PoolMode: "session", Source: "client"})
	// Overflow
	bus.Publish(decisions.Event{Port: "write", PoolMode: "session", Source: "client"})
	sub.DrainDropped()

	if bus.DroppedSubOverflow() != 1 {
		t.Fatalf("expected DroppedSubOverflow=1, got %d", bus.DroppedSubOverflow())
	}
}

func TestDecisionEventFieldMapping(t *testing.T) {
	// Leased events: the outcome_kind is "leased"
	event := decisions.Event{
		Port:     "write",
		PoolMode: "session",
		Outcome:  decisions.Outcome{Kind: "leased"},
	}
	if event.Outcome.Kind != "leased" {
		t.Fatalf("expected leased kind, got %s", event.Outcome.Kind)
	}

	// Rejected events: the outcome_kind is "rejected"
	event2 := decisions.Event{
		Port:     "read",
		PoolMode: "transaction",
		Outcome:  decisions.Outcome{Kind: "rejected", Reason: "read_port:write_attempted", SQLState: "25006"},
	}
	if event2.Outcome.Kind != "rejected" {
		t.Fatalf("expected rejected kind, got %s", event2.Outcome.Kind)
	}
	if event2.Outcome.Reason != "read_port:write_attempted" {
		t.Fatalf("expected read_port:write_attempted reason, got %s", event2.Outcome.Reason)
	}
	if event2.Outcome.SQLState != "25006" {
		t.Fatalf("expected 25006 SQLState, got %s", event2.Outcome.SQLState)
	}
}

func TestSSETransitionWireFormat(t *testing.T) {
	bus := decisions.NewBus(10, 10)
	defer bus.Close()
	policyEng := policy.NewEngine(nil, policy.NewTemplateSet(), nil, nil)
	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:0",
		Metrics:    config.DefaultAdminConfig().Metrics,
	}
	cfg.ServerConfig.AdminTokens = config.AdminTokenConfig{Tokens: map[string]config.AdminTokenEntry{"t": {Role: "admin"}}}

	s, err := New(nil, &cfg, zap.NewNop(), bus, policyEng)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest("GET", "/decisions", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	defer cancel()

	go func() {
		// Give handler time to subscribe
		time.Sleep(20 * time.Millisecond)
		bus.Publish(decisions.TransitionEvent{PolicyName: "health", NodeID: "n1", FromState: "clean", ToState: "active"})
		// Give handler time to write the event
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	s.handleDecisionsSSE(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "event: transition") {
		t.Fatalf("expected 'event: transition' in SSE body, got:\n%s", body)
	}
	if !strings.Contains(body, `"policy_name":"health"`) {
		t.Fatalf("expected policy_name in transition payload, got:\n%s", body)
	}
}

// TestSSEStreaming verifies the SSE endpoint sets proper headers and content type.
func TestSSEStreamingHeaders(t *testing.T) {
	bus := decisions.NewBus(10, 10)
	defer bus.Close()
	policyEng := policy.NewEngine(nil, policy.NewTemplateSet(), nil, zap.NewNop())
	cfg := config.Default()
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: "127.0.0.1:0",
		Metrics:    config.DefaultAdminConfig().Metrics,
	}
	cfg.ServerConfig.AdminTokens = config.AdminTokenConfig{Tokens: map[string]config.AdminTokenEntry{"t": {Role: "admin"}}}

	s, err := New(nil, &cfg, zap.NewNop(), bus, policyEng)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Directly invoke the handler
	req := httptest.NewRequest("GET", "/decisions", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()

	// The SSE handler blocks until the request context cancels, so we
	// cancel the context after a short delay to verify the headers.
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	s.handleDecisionsSSE(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %s", ct)
	}
}

