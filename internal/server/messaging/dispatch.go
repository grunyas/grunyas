package messaging

import (
	"context"

	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

// Process handles a single protocol message received from the client.
// Terminate messages must be handled by the session manager.
// It returns true when the message requires session-level pooling semantics.
func Process(ctx context.Context, msg pgproto3.FrontendMessage, upstream types.UpstreamClientInterface, logger *zap.Logger) (bool, error) {
	switch m := msg.(type) {
	case *pgproto3.Query:
		return queryUsesSessionState(m.String), ProcessSimpleQuery(m, upstream)
	case *pgproto3.Parse:
		// Named prepared statements persist in the backend connection's session
		// until explicitly closed. Pinning ensures subsequent Bind/Execute messages
		// reach the same backend connection where the statement was prepared.
		// Unnamed statements are transient (overwritten on next Parse) — no pin needed.
		return m.Name != "", ProcessParse(m, upstream)
	case *pgproto3.Bind:
		// Pin when binding a named prepared statement; the statement lives on a
		// specific backend connection and must be reachable for Execute.
		return m.PreparedStatement != "", ProcessBind(m, upstream)
	case *pgproto3.Describe:
		// Pin only for named statement Describes; portal Describes are transient.
		return m.ObjectType == 'S' && m.Name != "", ProcessDescribe(m, upstream)
	case *pgproto3.Execute:
		return false, ProcessExecute(m, upstream)
	case *pgproto3.Sync:
		return false, ProcessSync(m, upstream)
	case *pgproto3.Flush:
		return false, ProcessFlush(m, upstream)
	case *pgproto3.Close:
		return false, ProcessClose(m, upstream)
	default:
		logger.Warn("unsupported message type", zap.Any("message", m))
		return false, upstream.Send(m)
	}
}
