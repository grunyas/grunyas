package messaging

import (
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

// Process handles a single protocol message received from the client.
// Terminate messages must be handled by the session manager.
// It returns true when the message requires session-level pooling semantics.
//
// Extended-protocol Parse/Bind/Describe never pin: pgx auto-names statements
// (stmtcache_<hash>), so name-based pinning would permanently pin every
// connection without benefit. Persistent prepared statements arrive as
// a simple Query "PREPARE ..." which is handled above.
func Process(msg pgproto3.FrontendMessage, upstream types.UpstreamClientInterface, logger *zap.Logger) (bool, error) {
	switch m := msg.(type) {
	case *pgproto3.Query:
		return queryUsesSessionState(m.String), upstream.Send(m)
	case *pgproto3.Parse:
		return false, upstream.Send(m)
	case *pgproto3.Bind:
		return false, upstream.Send(m)
	case *pgproto3.Describe:
		return false, upstream.Send(m)
	case *pgproto3.Execute:
		return false, upstream.Send(m)
	case *pgproto3.Sync:
		return false, upstream.Send(m)
	case *pgproto3.Flush:
		return false, upstream.Send(m)
	case *pgproto3.Close:
		return false, upstream.Send(m)
	default:
		logger.Warn("unsupported message type", zap.Any("message", m))
		return false, upstream.Send(m)
	}
}
