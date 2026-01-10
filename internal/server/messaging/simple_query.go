package messaging

import (
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/grunyas/grunyas/internal/server/types"
)

func ProcessSimpleQuery(msg *pgproto3.Query, upstream types.UpstreamClientInterface) error {
	if err := upstream.Send(msg); err != nil {
		return err
	}

	return nil

	// reader, err := upstream.SendSimpleQuery(ctx, query)
	// if err != nil {
	// 	logger.Error("failed to process simple query", zap.Error(err))
	// 	return err
	// }

	// for reader.NextResult() {
	// 	fds := reader.FieldDescriptions()
	// 	if len(fds) > 0 {
	// 		if err := downstream.Send(&pgproto3.RowDescription{Fields: fds}); err != nil {
	// 			return err
	// 		}
	// 	}

	// 	for reader.NextRow() {
	// 		if err := downstream.Send(&pgproto3.DataRow{Values: reader.Values()}); err != nil {
	// 			return err
	// 		}
	// 	}

	// 	ct, err := reader.Close()
	// 	if err != nil {
	// 		logger.Warn("upstream query execution failed", zap.Error(err))

	// 		var pgErr *pgconn.PgError
	// 		if errors.As(err, &pgErr) {
	// 			if err := downstream.Send(&pgproto3.ErrorResponse{
	// 				Severity:         pgErr.Severity,
	// 				Code:             pgErr.Code,
	// 				Message:          pgErr.Message,
	// 				Detail:           pgErr.Detail,
	// 				Hint:             pgErr.Hint,
	// 				Position:         pgErr.Position,
	// 				InternalPosition: pgErr.InternalPosition,
	// 				InternalQuery:    pgErr.InternalQuery,
	// 				Where:            pgErr.Where,
	// 				SchemaName:       pgErr.SchemaName,
	// 				TableName:        pgErr.TableName,
	// 				ColumnName:       pgErr.ColumnName,
	// 				DataTypeName:     pgErr.DataTypeName,
	// 				ConstraintName:   pgErr.ConstraintName,
	// 				File:             pgErr.File,
	// 				Line:             pgErr.Line,
	// 				Routine:          pgErr.Routine,
	// 			}); err != nil {
	// 				return err
	// 			}
	// 		} else {
	// 			// If it's not a PgError, it's likely a fatal connection error or internal issue.
	// 			// We should report it and Close the session to ensure we don't reuse a bad connection.
	// 			if err := downstream.Send(&pgproto3.ErrorResponse{
	// 				Severity: "FATAL",
	// 				Code:     "58000", // system_error (or XX000)
	// 				Message:  fmt.Sprintf("upstream connection error: %v", err),
	// 			}); err != nil {
	// 				return err
	// 			}
	// 			return err
	// 		}
	// 	} else {
	// 		if err := downstream.Send(&ct); err != nil {
	// 			return err
	// 		}
	// 	}
	// }

	// return downstream.Send(&pgproto3.ReadyForQuery{TxStatus: upstream.TxStatus()})
}
