package policy

import (
	"sync"
	"time"

	"github.com/grunyas/grunyas/internal/topology"
	"go.uber.org/zap"
)

type State int

const (
	StateClean State = iota
	StatePending
	StateActive
	StateReleasing
)

func (s State) String() string {
	switch s {
	case StateClean:
		return "clean"
	case StatePending:
		return "pending"
	case StateActive:
		return "active"
	case StateReleasing:
		return "releasing"
	default:
		return "unknown"
	}
}

type Transition struct {
	PolicyName  string
	Template    string
	Scope       string
	NodeID      string
	From        State
	To          State
	Timestamp   time.Time
	Observation topology.NodeView
}

type CandidateState struct {
	NodeID          string
	State           State
	EnteredStateAt  time.Time
	LastCondition   string
}

type Instance struct {
	Name       string
	Template   string
	Scope      string
	Parameters map[string]int
	Timing     TemplateTiming
}

type TemplateTiming struct {
	DwellMs   int
	ReleaseMs int
}

type Evaluator interface {
	Evaluate(nv topology.NodeView, params map[string]int) (reject bool, reason string)
}

type templateSet struct {
	health Evaluator
	lag    Evaluator
}

type notificationBus interface {
	PublishTransition(t Transition)
}

type Engine struct {
	mu sync.RWMutex

	instances map[string]*Instance
	templates *templateSet
	bus       notificationBus
	logger    *zap.Logger

	states map[string]map[string]*perNodeState
	now    func() time.Time
}

type perNodeState struct {
	state         State
	enteredAt     time.Time
	lastCondition string
}

func NewEngine(instances []Instance, templates *templateSet, bus notificationBus, log *zap.Logger) *Engine {
	if log == nil {
		log = zap.NewNop()
	}
	e := &Engine{
		instances: make(map[string]*Instance),
		templates: templates,
		bus:       bus,
		logger:    log.With(zap.String("component", "policy")),
		states:    make(map[string]map[string]*perNodeState),
		now:       time.Now,
	}
	for i := range instances {
		inst := instances[i]
		e.instances[inst.Name] = &inst
		e.states[inst.Name] = make(map[string]*perNodeState)
	}
	e.ensureDefaults()
	return e
}

func (e *Engine) ensureDefaults() {
	if _, ok := e.instances[defaultHealthName]; !ok {
		e.instances[defaultHealthName] = &Instance{
			Name:     defaultHealthName,
			Template: TemplateHealthFilter,
			Scope:    "cluster",
			Timing: TemplateTiming{
				DwellMs:   5000,
				ReleaseMs: 5000,
			},
		}
		e.states[defaultHealthName] = make(map[string]*perNodeState)
	}
	if _, ok := e.instances[defaultLagName]; !ok {
		e.instances[defaultLagName] = &Instance{
			Name:     defaultLagName,
			Template: TemplateLagFilter,
			Scope:    "cluster",
			Parameters: map[string]int{
				"threshold_ms": 100,
			},
			Timing: TemplateTiming{
				DwellMs:   5000,
				ReleaseMs: 5000,
			},
		}
		e.states[defaultLagName] = make(map[string]*perNodeState)
	}
	if _, ok := e.instances[defaultSatPoolName]; !ok {
		e.instances[defaultSatPoolName] = &Instance{
			Name:     defaultSatPoolName,
			Template: TemplatePoolSaturation,
			Scope:    "cluster",
			Timing: TemplateTiming{
				DwellMs:   0,
				ReleaseMs: 0,
			},
		}
		e.states[defaultSatPoolName] = make(map[string]*perNodeState)
	}
}

func (e *Engine) EvaluateNode(nv topology.NodeView, poolSaturated bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for name, inst := range e.instances {
		var reject bool
		var reason string
		switch inst.Template {
		case TemplateHealthFilter:
			reject, reason = e.templates.health.Evaluate(nv, inst.Parameters)
		case TemplateLagFilter:
			reject, reason = e.templates.lag.Evaluate(nv, inst.Parameters)
		case TemplatePoolSaturation:
			reject, reason = poolRejectionMap(poolSaturated)
		default:
			continue
		}
		e.transition(name, inst, string(nv.ID), reject, reason, nv)
	}
}

func poolRejectionMap(saturated bool) (bool, string) {
	if saturated {
		return true, ReasonPoolSaturated
	}
	return false, ""
}

func (e *Engine) transition(policyName string, inst *Instance, nodeID string, reject bool, reason string, nv topology.NodeView) {
	pm := e.states[policyName]
	pns, ok := pm[nodeID]
	if !ok {
		pns = &perNodeState{state: StateClean}
		pm[nodeID] = pns
	}

	oldState := pns.state
	newState := oldState
	now := e.now()

	switch oldState {
	case StateClean:
		if reject {
			newState = StatePending
			pns.enteredAt = now
			pns.lastCondition = reason
		}
	case StatePending:
		if reject {
			dwell := time.Duration(inst.Timing.DwellMs) * time.Millisecond
			if now.Sub(pns.enteredAt) >= dwell {
				newState = StateActive
				pns.lastCondition = reason
			}
		} else {
			newState = StateClean
			pns.lastCondition = ""
		}
	case StateActive:
		if !reject {
			newState = StateReleasing
			pns.enteredAt = now
			pns.lastCondition = ""
		}
	case StateReleasing:
		if reject {
			newState = StateActive
			pns.lastCondition = reason
		} else {
			release := time.Duration(inst.Timing.ReleaseMs) * time.Millisecond
			if now.Sub(pns.enteredAt) >= release {
				newState = StateClean
				pns.lastCondition = ""
			}
		}
	}

	if newState != oldState {
		pns.state = newState
		if newState == StatePending || newState == StateActive || newState == StateReleasing {
			pns.enteredAt = now
		}
		if e.bus != nil {
			e.bus.PublishTransition(Transition{
				PolicyName:  policyName,
				Template:    inst.Template,
				Scope:       inst.Scope,
				NodeID:      nodeID,
				From:        oldState,
				To:          newState,
				Timestamp:   now,
				Observation: nv,
			})
		}
	}
}

func (e *Engine) IsEligible(policyName string, nodeID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if pm, ok := e.states[policyName]; ok {
		if pns, ok := pm[nodeID]; ok {
			return pns.state != StateActive && pns.state != StateReleasing
		}
	}
	return true
}

func (e *Engine) CandidateState(policyName, nodeID string) (State, time.Time, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if pm, ok := e.states[policyName]; ok {
		if pns, ok := pm[nodeID]; ok {
			return pns.state, pns.enteredAt, pns.lastCondition
		}
	}
	return StateClean, time.Time{}, ""
}

// EvaluatePolicyState returns the current eligibility state and reason
// for the given (policy, node) tuple in a single read-lock acquisition.
// Returns (eligible, reason). When eligible is true, reason is "".
// When eligible is false, reason is the non-empty exclusion reason.
func (e *Engine) EvaluatePolicyState(policyName string, nodeID string) (eligible bool, reason string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if pm, ok := e.states[policyName]; ok {
		if pns, ok := pm[nodeID]; ok {
			eligible = pns.state != StateActive && pns.state != StateReleasing
			reason = pns.lastCondition
			return
		}
	}
	return true, ""
}

func (e *Engine) Instances() []Instance {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]Instance, 0, len(e.instances))
	for _, inst := range e.instances {
		result = append(result, *inst)
	}
	return result
}

func (e *Engine) SetClock(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

func (e *Engine) CandidateStates(policyName string) map[string]CandidateState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string]CandidateState)
	if pm, ok := e.states[policyName]; ok {
		for nodeID, pns := range pm {
			result[nodeID] = CandidateState{
				NodeID:         nodeID,
				State:          pns.state,
				EnteredStateAt: pns.enteredAt,
				LastCondition:  pns.lastCondition,
			}
		}
	}
	return result
}
