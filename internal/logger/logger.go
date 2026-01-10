package logger

import (
	"context"
	"fmt"
	"io"

	"github.com/grunyas/grunyas/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Initialize configures structured logging and minimal telemetry (OTLP stubs).
// Returns the logger and a cleanup function for telemetry shutdown.
// If customWriter is provided, logs will be written to it instead of stdout/stderr.
func Initialize(ctx context.Context, logCfg config.LoggingConfig, telCfg config.TelemetryConfig, customWriter io.Writer) (*zap.Logger, func(context.Context) error, error) {
	level, err := zap.ParseAtomicLevel(logCfg.Level)
	if err != nil {
		return nil, nil, fmt.Errorf("parse log level: %w", err)
	}

	var zapCfg zap.Config
	if logCfg.Development {
		zapCfg = zap.NewDevelopmentConfig()
	} else {
		zapCfg = zap.NewProductionConfig()
	}
	zapCfg.Level = level

	var logger *zap.Logger
	if customWriter != nil {
		// Create a core that writes to the custom writer
		encoder := zapcore.NewJSONEncoder(zapCfg.EncoderConfig)
		if logCfg.Development {
			encoder = zapcore.NewConsoleEncoder(zapCfg.EncoderConfig)
		}
		core := zapcore.NewCore(encoder, zapcore.AddSync(customWriter), level)
		logger = zap.New(core)
	} else {
		logger, err = zapCfg.Build()
		if err != nil {
			return nil, nil, fmt.Errorf("build logger: %w", err)
		}
	}

	cleanup := func(context.Context) error { return nil }

	otelCleanup, err := setupTelemetry(ctx, telCfg)
	if err != nil {
		return nil, nil, err
	}
	if otelCleanup != nil {
		cleanup = otelCleanup
	}

	return logger, cleanup, nil
}
