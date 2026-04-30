//go:build integration
// +build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/admin"
	"github.com/grunyas/grunyas/internal/server/proxy"
	"github.com/grunyas/grunyas/internal/topology"
	"go.uber.org/zap"
)

func TestAdminHealthz(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", adminAddr))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf(`expected {"status":"ok"}, got %q`, body)
	}
}

func TestAdminStateWithPrimary(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	// Wait for at least one probe cycle so the primary is observed.
	time.Sleep(1500 * time.Millisecond)

	resp := authedGet(t, fmt.Sprintf("http://%s/state", adminAddr))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Cluster block
	cluster, ok := body["cluster"].(map[string]interface{})
	if !ok {
		t.Fatal("missing 'cluster' key")
	}
	sysID, ok := cluster["system_identifier"].(string)
	if !ok || sysID == "" {
		t.Fatal("expected non-empty system_identifier")
	}
	primary, ok := cluster["primary"].(string)
	if !ok || primary == "" {
		t.Fatal("expected non-empty primary node ID")
	}
	if primary != "primary-1" {
		t.Fatalf("expected primary='primary-1', got %q", primary)
	}
	if sb, ok := cluster["split_brain"].(bool); ok && sb {
		t.Fatal("expected split_brain=false")
	}

	// Nodes block
	nodes, ok := body["nodes"].([]interface{})
	if !ok || len(nodes) == 0 {
		t.Fatal("expected non-empty nodes array")
	}

	// Find the primary node and verify its shape.
	var primaryNode map[string]interface{}
	for _, n := range nodes {
		nm := n.(map[string]interface{})
		if nm["id"] == "primary-1" {
			primaryNode = nm
			break
		}
	}
	if primaryNode == nil {
		t.Fatal("primary-1 not found in nodes")
	}

	checkStringField(t, primaryNode, "liveness", "up")
	checkStringField(t, primaryNode, "observed_role", "primary")
	checkStringField(t, primaryNode, "declared_role", "primary")
	checkStringField(t, primaryNode, "liveness_state", "fresh")
	checkStringField(t, primaryNode, "replication_lag_state", "not_applicable")

	if _, ok := primaryNode["system_identifier"]; !ok {
		t.Fatal("missing system_identifier")
	}
	if match, ok := primaryNode["system_identifier_match"]; !ok || !match.(bool) {
		t.Fatal("expected system_identifier_match=true")
	}

	// Pools block — exactly 1 entry (primary only).
	pools, ok := body["pools"].([]interface{})
	if !ok {
		t.Fatal("missing 'pools' key")
	}
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool entry (primary only), got %d", len(pools))
	}
	poolEntry := pools[0].(map[string]interface{})
	checkStringField(t, poolEntry, "port", "write")
	checkStringField(t, poolEntry, "node_id", "primary-1")
	checkStringField(t, poolEntry, "mode", "session")
	if mc, ok := poolEntry["min_conns"].(float64); ok && mc <= 0 {
		t.Fatalf("expected min_conns > 0, got %f", mc)
	}
	if tc, ok := poolEntry["total_conns"].(float64); ok && tc < 0 {
		t.Fatalf("expected total_conns >= 0, got %f", tc)
	}

	if _, ok := body["policies"]; !ok {
		t.Fatal("missing 'policies' key")
	}
	if _, ok := body["observed_at"]; !ok {
		t.Fatal("missing 'observed_at' key")
	}
}

func TestAdminNodesEndpoint(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	time.Sleep(1500 * time.Millisecond)

	resp := authedGet(t, fmt.Sprintf("http://%s/nodes", adminAddr))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	nodes, ok := body["nodes"].([]interface{})
	if !ok {
		t.Fatal("missing 'nodes' key")
	}
	if len(nodes) == 0 {
		t.Fatal("expected at least 1 node")
	}
}

func TestAdminNodeByID(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	time.Sleep(1500 * time.Millisecond)

	resp := authedGet(t, fmt.Sprintf("http://%s/nodes/primary-1", adminAddr))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var node map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&node)
	if node["id"] != "primary-1" {
		t.Fatalf("expected id=primary-1, got %q", node["id"])
	}
}

func TestAdminNodeByIDNotFound(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	resp := authedGet(t, fmt.Sprintf("http://%s/nodes/nonexistent", adminAddr))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestAdminMetricsExposition(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", adminAddr))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") &&
		!strings.Contains(ct, "application/openmetrics-text") {
		t.Fatalf("expected Prometheus content type, got %q", ct)
	}

	// Verify M2 §5 metric names and label dimensions are present in the body.
	bodyBytes := readAll(t, resp.Body)
	body := string(bodyBytes)

	type metricLabel struct {
		family string
		labels []string
	}
	expected := []metricLabel{
		{"grunyas_nodes_total", nil},
		{"grunyas_nodes_by_role", []string{"role"}},
		{"grunyas_nodes_by_liveness", []string{"liveness"}},
		{"grunyas_node_liveness", []string{"node_id"}},
		{"grunyas_node_observed_role", []string{"node_id", "role"}},
		{"grunyas_node_role_disagreement", []string{"node_id"}},
		{"grunyas_node_observation_age_seconds", []string{"node_id", "property"}},
		{"grunyas_admin_requests_total", []string{"path", "method", "status"}},
		{"grunyas_admin_request_duration_seconds", []string{"path"}},
	}

	for _, m := range expected {
		if !strings.Contains(body, m.family+"{") && !strings.Contains(body, m.family+" ") {
			t.Errorf("metric family %q not found in exposition output", m.family)
			continue
		}
		for _, lbl := range m.labels {
			if !strings.Contains(body, lbl+"=") {
				t.Errorf("metric %q missing label %q in exposition output", m.family, lbl)
			}
		}
	}

	// Verify build info gauge is present.
	if !strings.Contains(body, "grunyas_build_info{") {
		t.Error("grunyas_build_info not found in exposition output")
	}
}

func TestAdminPoolsEndpoint(t *testing.T) {
	env := loadTestEnv(t)
	_, adminAddr, stop := startStack(t, env)
	defer stop()

	time.Sleep(1500 * time.Millisecond)

	resp := authedGet(t, fmt.Sprintf("http://%s/pools", adminAddr))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["pools"]; !ok {
		t.Fatal("missing 'pools' key")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// startStack starts proxy + admin and returns (proxyAddr, adminAddr, stop).
func startStack(t *testing.T, env testEnv) (string, string, func()) {
	t.Helper()

	waitForPostgres(t, env)

	cfg := stackConfig(t, env)

	ctx, cancel := context.WithCancel(context.Background())
	topo, err := topology.New(ctx, &cfg, zap.NewNop())
	if err != nil {
		cancel()
		t.Fatalf("topology.New: %v", err)
	}

	prx, err := proxy.Initialize(ctx, &cfg, zap.NewNop(), topo)
	if err != nil {
		t.Fatalf("proxy.Initialize: %v", err)
	}
	proxyErrCh := make(chan error, 1)
	go func() {
		proxyErrCh <- prx.Run()
	}()
	select {
	case <-prx.Ready():
	case err := <-proxyErrCh:
		cancel()
		t.Fatalf("proxy failed: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatalf("proxy startup timeout")
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	topo.WaitForInitialProbes(waitCtx)
	waitCancel()

	adm, err := admin.New(topo, &cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("admin.New: %v", err)
	}
	adminErrCh := make(chan error, 1)
	go func() {
		adminErrCh <- adm.Run(ctx)
	}()

	proxyAddr := cfg.ServerConfig.ListenAddr
	adminAddr := cfg.ServerConfig.Admin.ListenAddr

	// Wait for admin listener.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", adminAddr))
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	stop := func() {
		_ = adm.Close()
		cancel()
		<-proxyErrCh
		topo.Close()
	}

	return proxyAddr, adminAddr, stop
}

func stackConfig(t *testing.T, env testEnv) config.Config {
	t.Helper()
	proxyAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	adminAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	cfg := config.Default()
	cfg.ServerConfig.ListenAddr = proxyAddr
	cfg.ServerConfig.Ports = map[string]config.PortConfig{
		"write": {ListenAddr: proxyAddr, PoolMode: "session", SSLMode: "never"},
	}
	cfg.ServerConfig.Admin = config.AdminConfig{
		ListenAddr: adminAddr,
		Metrics:    config.DefaultAdminConfig().Metrics,
	}
	cfg.ServerConfig.AdminAddr = adminAddr
	cfg.ServerConfig.SSLMode = "never"
	cfg.ServerConfig.MaxSessions = 10
	cfg.ServerConfig.ClientIdleTimeout = 30

	cfg.Auth.Method = "plain"
	cfg.Auth.Username = env.user
	cfg.Auth.Password = env.password

	cfg.Nodes = []config.NodeConfig{
		{
			ID:           "primary-1",
			Host:         env.host,
			Port:         env.port,
			DeclaredRole: "primary",
			Connection: config.NodeConnectionConfig{
				User:                  env.user,
				Password:              env.password,
				Database:              env.database,
				ConnectTimeoutSeconds: 5,
			},
			Pool: config.NodePoolConfig{MinConns: 1, MaxConns: 4, HealthCheckPeriod: 60},
		},
	}
	cfg.ProbeConfig = config.DefaultProbeConfig()
	cfg.ServerConfig.AdminTokens = config.AdminTokenConfig{
		Tokens: map[string]config.AdminTokenEntry{
			"test-token": {Role: "admin"},
		},
	}
	cfg.Normalize()

	return cfg
}

func checkStringField(t *testing.T, m map[string]interface{}, key, want string) {
	t.Helper()
	got, ok := m[key].(string)
	if !ok {
		t.Fatalf("missing or non-string field %q", key)
	}
	if got != want {
		t.Fatalf("field %q: got %q, want %q", key, got, want)
	}
}

func authedGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	return b
}
