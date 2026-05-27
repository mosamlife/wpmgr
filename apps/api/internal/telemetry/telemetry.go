// Package telemetry initializes the OpenTelemetry tracer/meter providers with
// graceful shutdown (ADR-011). For the skeleton it defaults to a no-op exporter
// so no collector is required; set an OTLP endpoint to export over HTTP.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config controls telemetry initialization.
type Config struct {
	// ServiceName is reported as service.name on all spans.
	ServiceName string
	// OTLPEndpoint, when non-empty, enables the OTLP/HTTP trace exporter
	// pointing at that endpoint. When empty, traces are not exported (the
	// provider still records and propagates context — useful in tests/dev).
	OTLPEndpoint string
}

// Provider bundles the initialized providers and a shutdown hook.
type Provider struct {
	tracerProvider *sdktrace.TracerProvider
}

// Init configures the global tracer provider and propagators. The returned
// Provider's Shutdown must be called on exit to flush spans.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	// NewSchemaless (no schema URL) avoids a "conflicting Schema URL" error when
	// merging with resource.Default(), whose bundled semconv version may differ
	// from ours. Default()'s schema URL is kept; we only contribute attributes.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}

	if cfg.OTLPEndpoint != "" {
		exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("create otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Provider{tracerProvider: tp}, nil
}

// Shutdown flushes and stops the providers, bounded by a short timeout.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.tracerProvider == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return errors.Join(p.tracerProvider.Shutdown(ctx))
}
