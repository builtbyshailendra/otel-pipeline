package checkout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// The service's telemetry, captured in memory: what a collector would receive.
func TestTelemetryShape(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans)))
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	svc, err := New(Behaviour{ErrorRate: 0.1, SlowRate: 0.05, Slow: 20 * time.Millisecond, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(otelhttp.NewHandler(svc, "POST /checkout"))
	defer srv.Close()
	failed := 0
	for i := range 200 {
		resp, err := http.Post(srv.URL+"/checkout?user=u"+string(rune('a'+i%26)), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			failed++
		}
	}

	byTrace := map[string][]sdktrace.ReadOnlySpan{}
	for _, s := range spans.GetSpans().Snapshots() {
		byTrace[s.SpanContext().TraceID().String()] = append(byTrace[s.SpanContext().TraceID().String()], s)
	}
	if len(byTrace) != 200 {
		t.Fatalf("%d traces, want 200", len(byTrace))
	}
	errorTraces, slowTraces := 0, 0
	for id, ss := range byTrace {
		names := map[string]sdktrace.ReadOnlySpan{}
		var server sdktrace.ReadOnlySpan
		for _, s := range ss {
			names[s.Name()] = s
			// Found by kind, not name: current HTTP conventions name it
			// "{method} {route}", and "POST" alone when no route is known.
			if s.SpanKind() == trace.SpanKindServer {
				server = s
			}
		}
		if server == nil || names["load cart"] == nil || names["charge card"] == nil {
			t.Fatalf("trace %s is missing spans: %d", id, len(ss))
		}
		if server.Status().Code == codes.Error {
			errorTraces++
			if names["send receipt"] != nil {
				t.Fatalf("a failed checkout still sent a receipt")
			}
		} else if names["send receipt"] == nil {
			t.Fatalf("a successful checkout has no receipt span")
		}
		for _, e := range names["charge card"].Events() {
			if e.Name == "provider retried" {
				slowTraces++
			}
		}
		for _, a := range server.Attributes() {
			if a.Key == "payment.card" && len(a.Value.AsString()) < 15 {
				t.Fatalf("expected the raw card number on the span (the leak the collector must scrub)")
			}
		}
	}
	if errorTraces != failed || errorTraces == 0 || slowTraces == 0 {
		t.Fatalf("errors=%d (HTTP failures %d), slow=%d", errorTraces, failed, slowTraces)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var total int64
	exemplars := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					total += p.Value
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					exemplars += len(p.Exemplars)
				}
			}
		}
	}
	if total != 200 {
		t.Fatalf("checkout.requests = %d, want 200", total)
	}
	if exemplars == 0 {
		t.Fatal("the duration histogram should carry exemplars pointing at traces")
	}
	t.Logf("200 traces, %d failed, %d slow, %d exemplars", errorTraces, slowTraces, exemplars)
}
