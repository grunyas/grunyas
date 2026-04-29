package messaging

import (
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
)

func ProcessFlush(msg *pgproto3.Flush, upstream types.UpstreamClientInterface) error {
	if err := upstream.Send(msg); err != nil {
		return err
	}

	return nil
}
