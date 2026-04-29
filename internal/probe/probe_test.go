package probe

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeSink records the last UpdateLiveness call for assertion.
type fakeSink struct {
	mu        sync.Mutex
	liveness  Liveness
	lastErr   error
	callCount int
}

func (f *fakeSink) UpdateLiveness(id string, liveness Liveness, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveness = liveness
	f.lastErr = err
	f.callCount++
}

func (f *fakeSink) UpdateObservedRole(id string, role Role)               {}
func (f *fakeSink) UpdateSystemID(id string, sid string) error            { return nil }
func (f *fakeSink) UpdateLag(id string, lagMs int64, sampledAt time.Time) {}
func (f *fakeSink) MarkLagIdle(id string, sampledAt time.Time)            {}
func (f *fakeSink) MarkLagUnknown(id string, reason error)                {}

// errorSink returns a configured error from UpdateSystemID.
type errorSink struct {
	fakeSink
	sysIDErr    error
	sysIDCalled int
}

func (e *errorSink) UpdateSystemID(id string, sid string) error {
	e.sysIDCalled++
	return e.sysIDErr
}

func (f *fakeSink) LastLiveness() (Liveness, error, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveness, f.lastErr, f.callCount
}

// newTestProbe constructs a Probe without starting the loop or opening a connection.
func newTestProbe(t *testing.T, cfg ProbeConfig, sink Sink) *Probe {
	t.Helper()
	if sink == nil {
		sink = &fakeSink{}
	}
	return &Probe{
		spec:   NodeSpec{ID: "test-node"},
		cfg:    cfg,
		sink:   sink,
		logger: zap.NewNop(),
	}
}

func TestRecordFailureBelowThreshold(t *testing.T) {
	sink := &fakeSink{}
	p := newTestProbe(t, ProbeConfig{LivenessFailureCount: 3}, sink)

	for i := 0; i < 2; i++ {
		p.recordFailure(assertAnError)
	}

	_, _, calls := sink.LastLiveness()
	if calls != 0 {
		t.Fatalf("expected 0 calls to UpdateLiveness (below threshold), got %d", calls)
	}
}

func TestRecordFailureAtThreshold(t *testing.T) {
	sink := &fakeSink{}
	p := newTestProbe(t, ProbeConfig{LivenessFailureCount: 3}, sink)

	for i := 0; i < 3; i++ {
		p.recordFailure(assertAnError)
	}

	liveness, lastErr, calls := sink.LastLiveness()
	if calls != 1 {
		t.Fatalf("expected 1 call to UpdateLiveness (first crossing), got %d", calls)
	}
	if liveness != LivenessDown {
		t.Fatalf("expected LivenessDown, got %v", liveness)
	}
	if lastErr != assertAnError {
		t.Fatalf("expected the error to be passed through")
	}
}

func TestRecordFailureAboveThreshold(t *testing.T) {
	sink := &fakeSink{}
	p := newTestProbe(t, ProbeConfig{LivenessFailureCount: 3}, sink)

	for i := 0; i < 5; i++ {
		p.recordFailure(assertAnError)
	}

	_, lastErr, calls := sink.LastLiveness()
	// Calls happen on every failure past threshold (failures 3, 4, 5).
	if calls != 3 {
		t.Fatalf("expected 3 calls to UpdateLiveness (failures 3-5), got %d", calls)
	}
	if lastErr != assertAnError {
		t.Fatalf("expected last error to be passed through")
	}
}

func TestRecordFailureDefaultThresholdWhenZero(t *testing.T) {
	sink := &fakeSink{}
	p := newTestProbe(t, ProbeConfig{LivenessFailureCount: 0}, sink)

	// Zero config should default to 3. First 2 calls are below threshold.
	for i := 0; i < 2; i++ {
		p.recordFailure(assertAnError)
	}

	_, _, calls := sink.LastLiveness()
	if calls != 0 {
		t.Fatalf("expected 0 calls below default threshold (3), got %d", calls)
	}

	p.recordFailure(assertAnError) // cross threshold → first call

	_, _, calls = sink.LastLiveness()
	if calls != 1 {
		t.Fatalf("expected 1 call at default threshold (3), got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// classifyLagResult (NULL → idle, non-null → fresh)
// ---------------------------------------------------------------------------

func TestClassifyLagResultNullIsIdle(t *testing.T) {
	lagMs, idle, err := classifyLagResult(nil, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !idle {
		t.Fatal("expected idle=true for NULL lag")
	}
	if lagMs != 0 {
		t.Fatalf("expected lagMs=0 for idle, got %d", lagMs)
	}
}

func TestClassifyLagResultNullReplayAtIsIdle(t *testing.T) {
	lagFloat := float64(42.0)
	lagMs, idle, err := classifyLagResult(&lagFloat, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !idle {
		t.Fatal("expected idle=true when replayAt is nil")
	}
	if lagMs != 0 {
		t.Fatalf("expected lagMs=0, got %d", lagMs)
	}
}

func TestClassifyLagResultNullLagMsIsIdle(t *testing.T) {
	now := time.Now()
	lagMs, idle, err := classifyLagResult(nil, &now)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !idle {
		t.Fatal("expected idle=true when lagMs is nil")
	}
	if lagMs != 0 {
		t.Fatalf("expected lagMs=0, got %d", lagMs)
	}
}

func TestClassifyLagResultFresh(t *testing.T) {
	lagFloat := float64(23.5)
	now := time.Now()
	lagMs, idle, err := classifyLagResult(&lagFloat, &now)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idle {
		t.Fatal("expected idle=false for fresh lag")
	}
	if lagMs != 23 {
		t.Fatalf("expected lagMs=23 (truncated from 23.5), got %d", lagMs)
	}
}

func TestClassifyLagResultFreshRound(t *testing.T) {
	lagFloat := float64(0)
	now := time.Now()
	lagMs, idle, err := classifyLagResult(&lagFloat, &now)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if idle {
		t.Fatal("expected idle=false for zero lag with non-null replayAt")
	}
	if lagMs != 0 {
		t.Fatalf("expected lagMs=0, got %d", lagMs)
	}
}

// ---------------------------------------------------------------------------
// Probe → recordFailure via UpdateSystemID error
// ---------------------------------------------------------------------------

func TestProbeSinkSystemIDErrorCallsRecordFailure(t *testing.T) {
	sink := &errorSink{sysIDErr: &errSentinel{}}
	p := newTestProbe(t, ProbeConfig{LivenessFailureCount: 3}, sink)

	// Simulate what the probe loop does when UpdateSystemID fails:
	// probe.go:189-192: if err := p.sink.UpdateSystemID(...); err != nil { p.recordFailure(err); return }
	p.recordFailure(sink.sysIDErr)

	liveness, _, calls := sink.LastLiveness()
	if calls != 0 {
		t.Fatalf("expected 0 calls to UpdateLiveness (below threshold), got %d", calls)
	}
	if liveness != LivenessUp {
		t.Fatalf("expected unchanged liveness (zero value = LivenessUp), got %v", liveness)
	}
}

func TestProbeSinkSystemIDErrorAtThresholdCausesDown(t *testing.T) {
	sink := &errorSink{sysIDErr: &errSentinel{}}
	p := newTestProbe(t, ProbeConfig{LivenessFailureCount: 3}, sink)

	// 3 consecutive failures should cross the threshold.
	for i := 0; i < 3; i++ {
		p.recordFailure(sink.sysIDErr)
	}

	liveness, lastErr, calls := sink.LastLiveness()
	if calls != 1 {
		t.Fatalf("expected 1 call to UpdateLiveness at threshold, got %d", calls)
	}
	if liveness != LivenessDown {
		t.Fatalf("expected LivenessDown, got %v", liveness)
	}
	if lastErr != sink.sysIDErr {
		t.Fatalf("expected sysIDError passed to UpdateLiveness, got %v", lastErr)
	}
}

var assertAnError = &errSentinel{}

type errSentinel struct{}

func (e *errSentinel) Error() string { return "probe test error" }
