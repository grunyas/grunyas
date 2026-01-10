package pgx_log_adapter

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/tracelog"
	"go.uber.org/zap"
)

// PgxLogAdapter adapts a zap.Logger to the pgx tracelog.Logger interface.
type PgxLogAdapter struct {
	logger *zap.Logger
}

func Initialize(logger *zap.Logger) *PgxLogAdapter {
	return &PgxLogAdapter{logger: logger.With(zap.Namespace("pgx"))}
}

func (z *PgxLogAdapter) Log(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
	fields := make([]zap.Field, 0, len(data))
	for k, v := range data {
		fields = append(fields, zap.Any(k, v))
	}

	msg = strings.ToLower(msg)

	switch level {
	case tracelog.LogLevelTrace:
		z.logger.Debug(msg, append(fields, zap.String("pgx_level", "trace"))...)
	case tracelog.LogLevelDebug:
		z.logger.Debug(msg, fields...)
	case tracelog.LogLevelInfo:
		z.logger.Debug(msg, fields...)
	case tracelog.LogLevelWarn:
		z.logger.Debug(msg, fields...)
	case tracelog.LogLevelError:
		z.logger.Debug(msg, fields...)
	default:
		z.logger.Debug(msg, append(fields, zap.String("pgx_level", "unknown"))...)
	}
}
