// Package telemetry initializes the OpenTelemetry tracer/meter providers with
// graceful shutdown (ADR-011). For the skeleton it defaults to a no-op exporter
// so no collector is required; set an OTLP endpoint to export over HTTP.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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

// defaultTracesPath is the OTLP/HTTP path for the traces signal. The exporter
// applies it to a host:port endpoint but takes a URL's path verbatim, so for a
// URL we have to apply it ourselves.
const defaultTracesPath = "/v1/traces"

// resolveTracesEndpointURL returns the URL the OTLP/HTTP trace exporter should
// POST to, given an operator-configured endpoint.
//
// otlptracehttp.WithEndpointURL uses the configured URL's path as-is. An
// endpoint carrying no meaningful path — "http://collector:4318", the shape
// infra/docker-compose.yml ships — therefore resolves to the collector's root,
// where nothing accepts trace payloads. The exporter reports no error and the
// spans are simply dropped, so the failure is silent. (Older exporter releases
// left URLPath empty here and a later default-filling step substituted
// /v1/traces; newer ones pin it to "/" so that step no longer fires. Resolving
// the path ourselves makes the behaviour the same on either.)
//
// Appending the default path unconditionally is the opposite bug: an operator
// who configured ".../v1/traces" would get ".../v1/traces/v1/traces". So the
// default is applied only when the URL carries no path of its own, and an
// explicitly-configured path — including a custom one — is left untouched.
func resolveTracesEndpointURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.Opaque != "" {
		// Not a URL we can reason about (unparseable, scheme-relative, or an
		// opaque form like "collector:4318"). Hand it back unchanged so the
		// exporter's own validation and error reporting apply, rather than
		// inventing a URL the operator did not configure.
		return endpoint
	}
	if p := strings.TrimSpace(u.Path); p == "" || p == "/" {
		u.Path = defaultTracesPath
		return u.String()
	}
	return endpoint
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
		endpoint := resolveTracesEndpointURL(cfg.OTLPEndpoint)
		exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
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
