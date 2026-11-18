package scrubprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

var componentType = component.MustNewType("scrub")

const scopeName = "github.com/builtbyshailendra/otel-pipeline/scrubprocessor"

// NewFactory is what the collector builder registers.
func NewFactory() processor.Factory {
	return processor.NewFactory(componentType,
		func() component.Config { return createDefaultConfig() },
		processor.WithTraces(createTraces, component.StabilityLevelDevelopment),
		processor.WithLogs(createLogs, component.StabilityLevelDevelopment))
}

// It changes data in place, so the collector must hand it its own copy when
// a pipeline fans out to several consumers.
var mutates = processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true})

func createTraces(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
	s, err := newScrubber(cfg.(*Config), set.MeterProvider.Meter(scopeName))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewTraces(ctx, set, cfg, next, s.processTraces, mutates)
}

func createLogs(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
	s, err := newScrubber(cfg.(*Config), set.MeterProvider.Meter(scopeName))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewLogs(ctx, set, cfg, next, s.processLogs, mutates)
}
