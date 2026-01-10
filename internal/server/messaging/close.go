package messaging

import (
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/grunyas/grunyas/internal/server/types"
)

func ProcessClose(msg *pgproto3.Close, upstream types.UpstreamClientInterface) error {
	if err := upstream.Send(msg); err != nil {
		return err
	}

	return nil
}
