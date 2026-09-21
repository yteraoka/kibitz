package telemetry_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yteraoka/kibitz/internal/telemetry"
)

func TestSetupTracingWithAnEndpoint(t *testing.T) {
	// The exporter connects lazily, so this does not need a live collector;
	// what is being checked is that a misconfiguration fails at startup rather
	// than on the first span.
	shutdown, err := telemetry.SetupTracing(context.Background(), telemetry.TracingOptions{
		Endpoint:    "127.0.0.1:4317",
		Insecure:    true,
		Service:     "kibitz-test",
		Version:     "v0",
		SampleRatio: 0.5,
	})
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	_, span := telemetry.Tracer().Start(context.Background(), "test")
	span.End()

	// A real provider is installed, so the propagator has something to inject.
	if otel.GetTextMapPropagator() == nil {
		t.Error("no propagator was installed")
	}
}

func TestCarrierRoundTrip(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(tracetest.NewInMemoryExporter()),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider) })

	ctx, span := telemetry.Tracer().Start(context.Background(), "producer")
	defer span.End()

	carrier := telemetry.Carrier{}
	telemetry.InjectTrace(ctx, carrier)
	if carrier["traceparent"] == "" {
		t.Fatal("nothing was injected")
	}
	if got := carrier.Get("traceparent"); got != carrier["traceparent"] {
		t.Error("Get does not read what Set wrote")
	}
	if len(carrier.Keys()) == 0 {
		t.Error("Keys returned nothing")
	}

	// The extracted context has to point at the same trace, which is the whole
	// point of carrying it through the queue.
	restored := telemetry.ExtractTrace(context.Background(), carrier)
	_, child := telemetry.Tracer().Start(restored, "consumer")
	defer child.End()

	if child.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Errorf("the consumer joined trace %s, want %s",
			child.SpanContext().TraceID(), span.SpanContext().TraceID())
	}
}
