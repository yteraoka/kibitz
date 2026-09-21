package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracingOptions configures the tracer.
type TracingOptions struct {
	// Endpoint is an OTLP gRPC collector, such as localhost:4317. When it is
	// empty tracing is off and every span becomes a no-op, which is what keeps
	// the instrumentation free to leave in the code.
	Endpoint string
	// Insecure sends to the collector without TLS, for a sidecar collector on
	// the same host.
	Insecure bool
	Service  string
	Version  string
	// SampleRatio is the fraction of traces recorded. Zero means record
	// everything: a review is rare and expensive enough that every one of them
	// is worth a trace.
	SampleRatio float64
}

// Shutdown flushes pending spans. It is called before the process exits, or
// the last few traces of a shutting-down worker are lost.
type Shutdown func(context.Context) error

// SetupTracing installs the global tracer provider and the W3C propagator.
//
// The propagator matters as much as the provider: the trace has to survive the
// hop through the queue, which is a message rather than an HTTP request, so
// the context travels as traceparent inside the event itself.
func SetupTracing(ctx context.Context, opts TracingOptions) (Shutdown, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if opts.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	clientOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(opts.Endpoint)}
	if opts.Insecure {
		clientOpts = append(clientOpts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: connecting to the trace collector: %w", err)
	}

	// The semconv version above has to match the one the SDK builds its
	// default resource with, or this merge fails on conflicting schema URLs.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.Service),
		semconv.ServiceVersion(opts.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: describing this service: %w", err)
	}

	sampler := sdktrace.AlwaysSample()
	if opts.SampleRatio > 0 && opts.SampleRatio < 1 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.SampleRatio))
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(provider)

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return provider.Shutdown(ctx)
	}, nil
}

// Tracer returns the tracer kibitz records spans on.
func Tracer() trace.Tracer { return otel.Tracer("github.com/yteraoka/kibitz") }

// Carrier adapts a string map to the OpenTelemetry propagation interface, so
// trace context can ride inside a queue message instead of an HTTP header.
type Carrier map[string]string

// Get implements [propagation.TextMapCarrier].
func (c Carrier) Get(key string) string { return c[key] }

// Set implements [propagation.TextMapCarrier].
func (c Carrier) Set(key, value string) { c[key] = value }

// Keys implements [propagation.TextMapCarrier].
func (c Carrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// InjectTrace writes the current trace context into a carrier.
func InjectTrace(ctx context.Context, carrier Carrier) {
	otel.GetTextMapPropagator().Inject(ctx, carrier)
}

// ExtractTrace reads trace context out of a carrier, so a job continues the
// trace the webhook started rather than beginning a new one.
func ExtractTrace(ctx context.Context, carrier Carrier) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// Attributes used on kibitz's own spans.
const (
	AttrPlatform    = attribute.Key("kibitz.platform")
	AttrKind        = attribute.Key("kibitz.kind")
	AttrRepository  = attribute.Key("kibitz.repository")
	AttrPullRequest = attribute.Key("kibitz.pull_request")
	AttrEventID     = attribute.Key("kibitz.event_id")
	AttrFindings    = attribute.Key("kibitz.findings")
)
