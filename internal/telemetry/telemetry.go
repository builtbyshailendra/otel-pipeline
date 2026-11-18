// Package telemetry wires the OpenTelemetry SDK: traces, metrics and logs,
// all exported over OTLP/HTTP to a collector (OTEL_EXPORTER_OTLP_ENDPOINT,
// default http://localhost:4318).
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Setup starts the three signal pipelines for a service. HEAD_SAMPLE_RATIO
// (0..1, default 1) sets head sampling: the decision made when a trace
// starts, before anyone knows whether it will fail.
func Setup(ctx context.Context, service string) (func(context.Context) error, error) {
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(service), semconv.ServiceVersion("1.0.0"),
		semconv.DeploymentEnvironmentName("lab")))
	if err != nil {
		return nil, err
	}

	ratio := 1.0
	if v, err := strconv.ParseFloat(os.Getenv("HEAD_SAMPLE_RATIO"), 64); err == nil {
		ratio = v
	}
	spanExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(spanExp, sdktrace.WithBatchTimeout(200*time.Millisecond)),
		// ParentBased: a downstream service follows the caller's decision,
		// so a trace is kept or dropped as a whole.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(time.Second))))

	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp, sdklog.WithExportInterval(200*time.Millisecond))))

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	global.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	// slog calls become OTel log records carrying the active trace and span IDs.
	slog.SetDefault(otelslog.NewLogger(service))

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}, nil
}
