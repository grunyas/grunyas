package routing

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grunyas/grunyas/internal/classifier"
	"github.com/grunyas/grunyas/internal/decisions"
	"github.com/grunyas/grunyas/internal/policy"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/grunyas/grunyas/internal/topology"
	"go.uber.org/zap"
)

type LeaseRequest struct {
	Port         string
	PoolMode     string
	LeaseType    string
	SQL          string
	PoolSaturated map[topology.NodeID]bool
}

type LeaseResult struct {
	Upstream  types.UpstreamClientInterface
	NodeID    topology.NodeID
	Decision  decisions.Event
	Error     error
}

type Pipeline struct {
	topo        *topology.Topology
	policyEng   *policy.Engine
	decisionsBus *decisions.Bus
	logger      *zap.Logger

	readRR atomic.Uint64

	DecisionsTotal    atomic.Int64
	DecisionsLeased   atomic.Int64
	DecisionsRejected atomic.Int64
	PublishedTotal    atomic.Int64
	EligibleSetRead   atomic.Int64
	EligibleSetWrite  atomic.Int64

	decisionCounters sync.Map // key: "port:outcome:reason", value: *atomic.Int64

	decisionDurationHistograms sync.Map // key: port, value: *portHistogram
}

// Bucket boundaries for grunyas_routing_decision_duration_seconds.
const durationBucketCount = 8

var durationBucketBounds = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5}

type portHistogram struct {
	sum     atomic.Int64
	count   atomic.Int64
	buckets [durationBucketCount]atomic.Int64
}

func (h *portHistogram) observe(d time.Duration) {
	sec := d.Seconds()
	h.sum.Add(int64(d))
	h.count.Add(1)
	for i, bound := range durationBucketBounds {
		if sec <= bound {
			h.buckets[i].Add(1)
		}
	}
}

func NewPipeline(topo *topology.Topology, policyEng *policy.Engine, bus *decisions.Bus, log *zap.Logger) *Pipeline {
	return &Pipeline{
		topo:         topo,
		policyEng:    policyEng,
		decisionsBus: bus,
		logger:       log.With(zap.String("component", "routing")),
	}
}

func (p *Pipeline) Bus() *decisions.Bus {
	return p.decisionsBus
}

func (p *Pipeline) PolicyEng() *policy.Engine {
	return p.policyEng
}

func (p *Pipeline) Lease(req LeaseRequest) (*LeaseResult, error) {
	p.DecisionsTotal.Add(1)

	var result LeaseResult
	event := decisions.Event{
		Port:      req.Port,
		PoolMode:  req.PoolMode,
		LeaseType: req.LeaseType,
		Source:    "client",
	}

	if !p.isValidPort(req.Port) {
		result.Error = &types.ProxyError{Code: "0A000", Message: "[grunyas] compat port classification not yet implemented (M4)"}
		event.Outcome = decisions.Outcome{Kind: "rejected", SQLState: "0A000", Reason: "compat_port:m4_stub"}
		p.DecisionsRejected.Add(1)
		result.Decision = event
		p.emitEvent(event)
		return &result, nil
	}

	start := time.Now()
	defer func() {
		loadOrStoreHistogram(&p.decisionDurationHistograms, req.Port).observe(time.Since(start))
	}()

	cls, clsSource := p.classify(req.Port, req.SQL)
	event.Classification = decisions.Classification{
		Type:   string(cls),
		Source: clsSource,
		SQL:    classifier.TruncateSQL(req.SQL, 256),
	}

	nodes := p.topo.Nodes()

	if len(nodes) == 0 {
		result.Error = &types.ProxyError{Code: "57P03", Message: "[grunyas] no upstream available"}
		event.Outcome = decisions.Outcome{Kind: "rejected", SQLState: "57P03", Reason: "no_nodes"}
		p.DecisionsRejected.Add(1)
		result.Decision = event
		p.emitEvent(event)
		return &result, nil
	}

	candidates := p.filterCandidates(nodes, req.Port)
	event.Candidates = candidates

	activePolicies := []string{}
	for _, inst := range p.policyEng.Instances() {
		for _, c := range candidates {
			if state, _, _ := p.policyEng.CandidateState(inst.Name, c.NodeID); state == policy.StateActive {
				activePolicies = append(activePolicies, inst.Name)
				break
			}
		}
	}
	event.PoliciesActive = activePolicies

	eligible := eligibleNodes(candidates, req.Port)

	if req.Port == "read" {
		p.EligibleSetRead.Store(int64(len(eligible)))
	} else {
		p.EligibleSetWrite.Store(int64(len(eligible)))
	}

	if len(eligible) == 0 {
		reason := "no_eligible_replica"
		code := "57P03"
		msg := "[grunyas] no eligible replica available"
		if req.Port == "write" {
			reason = "no_primary"
			msg = "[grunyas] no primary available"
		}
		result.Error = &types.ProxyError{Code: code, Message: msg}
		event.Outcome = decisions.Outcome{Kind: "rejected", SQLState: code, Reason: reason}
		switch req.Port {
		case "read":
			event.Consistency = &decisions.Consistency{Mode: "bounded_staleness"}
		case "write":
			event.Consistency = &decisions.Consistency{Mode: "linearizable"}
		}
		p.DecisionsRejected.Add(1)
		result.Decision = event
		p.emitEvent(event)
		return &result, nil
	}

	primary, primaryOK := p.topo.Primary()

	// Split-brain check: refuse writes when no single primary is observed.
	if req.Port == "write" {
		if !primaryOK {
			result.Error = &types.ProxyError{Code: "57P03", Message: "[grunyas] no primary available"}
			event.Outcome = decisions.Outcome{Kind: "rejected", SQLState: "57P03", Reason: "no_primary"}
			event.Consistency = &decisions.Consistency{Mode: "linearizable"}
			p.DecisionsRejected.Add(1)
			p.emitEvent(event)
			return &result, nil
		}
	}

	chosen := p.selectNode(eligible, req.Port, primary, primaryOK)
	event.Outcome = decisions.Outcome{
		Kind:   "leased",
		NodeID: string(chosen),
	}
	p.DecisionsLeased.Add(1)
	result.NodeID = chosen

	mgr, err := p.topo.PoolFor(chosen)
	if err != nil {
		result.Error = &types.ProxyError{Code: "57P03", Message: "[grunyas] no upstream available", Cause: err}
		event.Outcome = decisions.Outcome{Kind: "rejected", SQLState: "57P03", Reason: "pool_lookup_failed"}
		p.DecisionsRejected.Add(1)

		// The final outcome is rejected, not leased. Correct the aggregate so
		// leased + rejected stays consistent with total.
		p.DecisionsLeased.Add(-1)
		result.Decision = event
		p.emitEvent(event)
		return &result, nil
	}

	upstream, err := mgr.AcquireDbConnection()
	if err != nil {
		result.Error = &types.ProxyError{Code: "53300", Message: "[grunyas] pool saturated", Cause: err}
		event.Outcome = decisions.Outcome{Kind: "rejected", SQLState: "53300", Reason: "pool:acquisition_failed"}
		p.DecisionsRejected.Add(1)
		p.DecisionsLeased.Add(-1)
		result.Decision = event
		p.emitEvent(event)
		return &result, nil
	}

	result.Upstream = upstream

	switch req.Port {
	case "read":
		event.Consistency = &decisions.Consistency{Mode: "bounded_staleness"}
	case "write":
		event.Consistency = &decisions.Consistency{Mode: "linearizable"}
	}

	p.emitEvent(event)
	return &result, nil
}

func (p *Pipeline) updatePolicyEvaluation(poolSaturated map[topology.NodeID]bool) {
	if p.topo == nil {
		return
	}
	nodes := p.topo.Nodes()

	// Build saturation map from pool stats when caller doesn't provide one.
	if poolSaturated == nil {
		poolSaturated = make(map[topology.NodeID]bool)
		for _, nv := range nodes {
			mgr, err := p.topo.PoolFor(nv.ID)
			if err != nil {
				continue
			}
			stats := mgr.PoolStats()
			poolSaturated[nv.ID] = stats.AcquiredConns >= stats.MaxConns && stats.MaxConns > 0
		}
	}

	for _, nv := range nodes {
		saturated := false
		if poolSaturated != nil {
			saturated = poolSaturated[nv.ID]
		}
		p.policyEng.EvaluateNode(nv, saturated)
	}
}

// StartObservationLoop runs a background goroutine that periodically
// evaluates all policies against the current topology observations.
// The loop runs on the given interval and stops when ctx is done.
// This ensures policies are evaluated even under zero traffic,
// satisfying the observation-driven model from M3.md §5.
func (p *Pipeline) StartObservationLoop(ctx context.Context, intervalMs int) {
	if intervalMs <= 0 {
		intervalMs = 1000
	}
	if intervalMs < 100 {
		intervalMs = 100
	}
	ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
	go func() {
		p.logger.Info("observation-driven policy evaluation started",
			zap.Int("interval_ms", intervalMs))
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				p.updatePolicyEvaluation(nil)
			}
		}
	}()
}

func (p *Pipeline) isValidPort(port string) bool {
	return port == "write" || port == "read"
}

func (p *Pipeline) classify(port, sql string) (classifier.ClassType, string) {
	c := classifier.NewPortClassifier(port)
	return c.Classify(sql)
}

// filterCandidates evaluates every node against the full eligibility chain.
// Each filter runs exactly once per node and either produces a reason or not.
// Reason strings are drawn from a rule-3 committed vocabulary.
func (p *Pipeline) filterCandidates(nodes []topology.NodeView, port string) []decisions.Candidate {
	var clusterID topology.SystemID
	if p.topo != nil {
		clusterID = p.topo.ClusterID()
	}

	var hName, lName, sName string
	if p.policyEng != nil {
		hName = policyDefaultHealthNameOrFirst(p.policyEng)
		lName = policyDefaultLagNameOrFirst(p.policyEng)
		sName = policyDefaultSatNameOrFirst(p.policyEng)
	}

	candidates := make([]decisions.Candidate, 0, len(nodes))
	for _, nv := range nodes {
		var hEl, lEl, sEl bool
		var hCond, lCond, sCond string
		if p.policyEng != nil {
			hEl, hCond = p.policyEng.EvaluatePolicyState(hName, string(nv.ID))
			lEl, lCond = p.policyEng.EvaluatePolicyState(lName, string(nv.ID))
			sEl, sCond = p.policyEng.EvaluatePolicyState(sName, string(nv.ID))
		} else {
			hEl, lEl, sEl = true, true, true
		}

		c := decisions.Candidate{NodeID: string(nv.ID)}
		var reasons []string

		if nv.SystemID != "" && nv.SystemID != clusterID {
			reasons = append(reasons, "system_id:mismatch")
		}
		if port == "write" && nv.ObservedRole != topology.RolePrimary {
			if nv.ObservedRole == topology.RoleUnknown {
				reasons = append(reasons, "role:unknown")
			} else {
				reasons = append(reasons, "role:not_primary")
			}
		} else if port == "read" && nv.ObservedRole != topology.RoleReplica {
			if nv.ObservedRole == topology.RoleUnknown {
				reasons = append(reasons, "role:unknown")
			} else {
				reasons = append(reasons, "role:not_replica")
			}
		}
		if !hEl {
			if hCond != "" {
				reasons = append(reasons, hCond)
			} else {
				reasons = append(reasons, "liveness:not_up")
			}
		}
		if !lEl {
			if lCond != "" {
				reasons = append(reasons, lCond)
			} else {
				reasons = append(reasons, "lag:policy_active")
			}
		}
		if !sEl {
			if sCond != "" {
				reasons = append(reasons, sCond)
			} else {
				reasons = append(reasons, "pool:saturated")
			}
		}

		if len(reasons) > 0 {
			c.Eligible = false
			c.Reasons = reasons
		} else {
			c.Eligible = true
		}
		candidates = append(candidates, c)
	}
	return candidates
}

func eligibleNodes(candidates []decisions.Candidate, port string) []topology.NodeID {
	var result []topology.NodeID
	for _, c := range candidates {
		if c.Eligible {
			result = append(result, topology.NodeID(c.NodeID))
		}
	}
	return result
}

func (p *Pipeline) selectNode(eligible []topology.NodeID, port string, primary topology.NodeView, primaryOK bool) topology.NodeID {
	if port == "write" {
		if primaryOK {
			for _, id := range eligible {
				if id == primary.ID {
					return id
				}
			}
		}
		// Primary not found among eligible — this is a safety fallback.
		// The upstream split-brain check should have already rejected, so this
		// path is dead in normal operation. Pick the first eligible by ID sort
		// so the selection is deterministic and replays identically.
		p.logger.Warn("selectNode: write-port eligible[0] fallback — split-brain guard should have fired",
			zap.Any("eligible", eligible))
		selected := eligible[0]
		for _, id := range eligible {
			if id < selected {
				selected = id
			}
		}
		return selected
	}
	if len(eligible) == 1 {
		return eligible[0]
	}
	idx := p.readRR.Add(1) % uint64(len(eligible))
	return eligible[idx]
}

func (p *Pipeline) emitEvent(event decisions.Event) {
	if p.decisionsBus == nil {
		return
	}
	p.PublishedTotal.Add(1)

	port := event.Port
	if !p.isValidPort(port) {
		port = "invalid"
	}
	key := port + ":" + event.Outcome.Kind + ":" + event.Outcome.Reason
	loadOrStoreInt64(&p.decisionCounters, key).Add(1)

	p.decisionsBus.Publish(event)
}

func policyDefaultHealthNameOrFirst(eng *policy.Engine) string {
	for _, inst := range eng.Instances() {
		if inst.Template == policy.TemplateHealthFilter {
			return inst.Name
		}
	}
	return "default-health-filter"
}

func policyDefaultLagNameOrFirst(eng *policy.Engine) string {
	for _, inst := range eng.Instances() {
		if inst.Template == policy.TemplateLagFilter {
			return inst.Name
		}
	}
	return "default-lag-filter"
}

func policyDefaultSatNameOrFirst(eng *policy.Engine) string {
	for _, inst := range eng.Instances() {
		if inst.Template == policy.TemplatePoolSaturation {
			return inst.Name
		}
	}
	return "default-pool-saturation-rejection"
}

func (p *Pipeline) IsEligibleReadPort(nv topology.NodeView) bool {
	hName := policyDefaultHealthNameOrFirst(p.policyEng)
	lName := policyDefaultLagNameOrFirst(p.policyEng)
	sName := policyDefaultSatNameOrFirst(p.policyEng)
	hOK, _ := p.policyEng.EvaluatePolicyState(hName, string(nv.ID))
	if !hOK {
		return false
	}
	lOK, _ := p.policyEng.EvaluatePolicyState(lName, string(nv.ID))
	if !lOK {
		return false
	}
	sOK, _ := p.policyEng.EvaluatePolicyState(sName, string(nv.ID))
	return sOK
}

func (p *Pipeline) DecisionCountersSnapshot() map[string]int64 {
	result := make(map[string]int64)
	p.decisionCounters.Range(func(key, value interface{}) bool {
		result[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})
	return result
}

func (p *Pipeline) DecisionDurationHistogramSnapshot() map[string]map[float64]uint64 {
	result := make(map[string]map[float64]uint64)
	p.decisionDurationHistograms.Range(func(key, value interface{}) bool {
		port := key.(string)
		h := value.(*portHistogram)
		buckets := make(map[float64]uint64, len(durationBucketBounds))
		for i, bound := range durationBucketBounds {
			buckets[bound] = uint64(h.buckets[i].Load())
		}
		result[port] = buckets
		return true
	})
	return result
}

func (p *Pipeline) DecisionDurationSumSnapshot() map[string]int64 {
	result := make(map[string]int64)
	p.decisionDurationHistograms.Range(func(key, value interface{}) bool {
		result[key.(string)] = value.(*portHistogram).sum.Load()
		return true
	})
	return result
}

func (p *Pipeline) DecisionDurationCountSnapshot() map[string]int64 {
	result := make(map[string]int64)
	p.decisionDurationHistograms.Range(func(key, value interface{}) bool {
		result[key.(string)] = value.(*portHistogram).count.Load()
		return true
	})
	return result
}

func loadOrStoreInt64(m *sync.Map, key string) *atomic.Int64 {
	v, ok := m.Load(key)
	if ok {
		return v.(*atomic.Int64)
	}
	actual, _ := m.LoadOrStore(key, new(atomic.Int64))
	return actual.(*atomic.Int64)
}

func loadOrStoreHistogram(m *sync.Map, key string) *portHistogram {
	v, ok := m.Load(key)
	if ok {
		return v.(*portHistogram)
	}
	actual, _ := m.LoadOrStore(key, new(portHistogram))
	return actual.(*portHistogram)
}
