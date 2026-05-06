package logger

import (
	"context"
	"encoding/json"

	"github.com/grunyas/grunyas/internal/decisions"
	"go.uber.org/zap"
)

// StartDecisionExporter subscribes to the decisions bus and logs every
// routing decision and policy transition as a structured log record through
// the provided logger.
//
// Severity mirrors the outcome for decisions:
//   - INFO  for outcome.kind = "leased"
//   - WARN  for outcome.kind = "rejected" or "fallback"
//
// Policy transitions are always logged at INFO severity.
//
// The exporter runs until ctx is canceled. If the bus subscriber falls
// behind, events are dropped and the bus drop counter is incremented.
func StartDecisionExporter(ctx context.Context, bus *decisions.Bus, log *zap.Logger) {
	sub, ok := bus.Subscribe()
	if !ok {
		log.Warn("decision exporter: failed to subscribe to bus (max subscribers reached)")
		return
	}

	go func() {
		defer sub.Unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-sub.Ch:
				if !ok {
					return
				}
				if drops := sub.DrainDropped(); drops > 0 {
					bus.MarkOTelDropped(drops)
					log.Warn("decision exporter: dropped events", zap.Int64("count", drops))
				}
				switch event := msg.(type) {
				case decisions.Event:
					fields := []zap.Field{
						zap.String("port", event.Port),
						zap.String("pool_mode", event.PoolMode),
						zap.String("outcome_kind", event.Outcome.Kind),
						zap.String("event_id", event.EventID),
					}
					if event.Outcome.NodeID != "" {
						fields = append(fields, zap.String("outcome_node_id", event.Outcome.NodeID))
					}
					if event.Outcome.Reason != "" {
						fields = append(fields, zap.String("outcome_reason", event.Outcome.Reason))
					}
					body, _ := jsonMarshal(event)
					fields = append(fields, zap.String("decision_event", body))

					if event.Outcome.Kind == "rejected" || event.Outcome.Kind == "fallback" {
						log.Warn("routing decision", fields...)
					} else {
						log.Info("routing decision", fields...)
					}
				case decisions.TransitionEvent:
					body, _ := jsonMarshal(event)
					log.Info("policy transition",
						zap.String("event_id", event.EventID),
						zap.String("policy_name", event.PolicyName),
						zap.String("node_id", event.NodeID),
						zap.String("from_state", event.FromState),
						zap.String("to_state", event.ToState),
						zap.String("transition_event", body),
					)
				}
			}
		}
	}()
}

func jsonMarshal(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
