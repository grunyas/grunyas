package messaging

import (
	"context"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/grunyas/grunyas/internal/server/types"
	"go.uber.org/zap"
)

// Process handles a single protocol message received from the client.
// Terminate messages must be handled by the session manager.
func Process(ctx context.Context, msg pgproto3.FrontendMessage, upstream types.UpstreamClientInterface, downstream types.DownstreamClientInterface, logger *zap.Logger) error {
	switch m := msg.(type) {
	case *pgproto3.Query:
		return ProcessSimpleQuery(m, upstream)
	case *pgproto3.Parse:
		return ProcessParse(m, upstream)
	case *pgproto3.Bind:
		return ProcessBind(m, upstream)
	case *pgproto3.Describe:
		return ProcessDescribe(m, upstream)
	case *pgproto3.Execute:
		return ProcessExecute(m, upstream)
	case *pgproto3.Sync:
		return ProcessSync(m, upstream)
	case *pgproto3.Flush:
		return ProcessFlush(m, upstream)
	case *pgproto3.Close:
		return ProcessClose(m, upstream)
	default:
		logger.Warn("unsupported message type", zap.Any("message", m))
		return upstream.Send(m)
	}
}
