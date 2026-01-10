package logger

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/grunyas/grunyas/config"
)

// setupTelemetry configures a minimal OTLP trace exporter and tracer provider.
// Returns a shutdown function or nil if telemetry is disabled.
func setupTelemetry(ctx context.Context, telCfg config.TelemetryConfig) (func(context.Context) error, error) {
	if telCfg.OTLPEndpoint == "" {
		return nil, nil
	}

	clientOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(telCfg.OTLPEndpoint),
	}
	if telCfg.Insecure {
		clientOpts = append(clientOpts, otlptracegrpc.WithInsecure())
	}

	traceExp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(clientOpts...))
	if err != nil {
		return nil, fmt.Errorf("create otlp trace exporter: %w", err)
	}

	res, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(telCfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return tp.Shutdown, nil
}
