package logger

import (
	"context"
	"encoding/json"

	"github.com/grunyas/grunyas/internal/decisions"
	"go.uber.org/zap"
)

// StartDecisionExporter subscribes to the decisions bus and logs every
// routing decision as a structured log record through the provided logger.
//
// Severity mirrors the outcome:
//   - INFO  for outcome.kind = "leased"
//   - WARN  for outcome.kind = "rejected" or "fallback"
//
// The event body is serialized to JSON in the "decision_event" field.
// Mirrored OTel attributes (port, pool_mode, outcome_kind, outcome_node_id)
// are exposed as separate zap fields.
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
				event, ok := msg.(decisions.Event)
				if !ok {
					continue
				}
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
