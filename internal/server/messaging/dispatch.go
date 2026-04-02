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
		// Extended-protocol Parse messages (named or unnamed) are NOT SQL PREPARE — they
		// don't create persistent session state. SQL PREPARE arrives as a Query message.
		// Never pin for Parse.
		return false, ProcessParse(m, upstream)
	case *pgproto3.Bind:
		return false, ProcessBind(m, upstream)
	case *pgproto3.Describe:
		return false, ProcessDescribe(m, upstream)
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
