# Learning notes: otel-pipeline

## 1. OpenTelemetry in five minutes

OpenTelemetry (OTel) is the vendor-neutral standard for telemetry. Instrument once, send anywhere.

- **Three signals.** *Traces*: a request's journey as a tree of spans (each with a start, an end, attributes, events and a status). *Metrics*: numbers over time (counters, histograms). *Logs*: records with a body and attributes. Logs and metrics can carry trace and span IDs, so you can jump from a slow-bucket metric (an **exemplar**) or an error log straight to its trace.
- **The SDK** in each service creates the data and batches it; an **exporter** sends it over **OTLP** (OTel's protocol, gRPC or HTTP).
- **Context propagation**: the `traceparent` header carries the trace ID between services, so `loadgen → checkout` is one trace.
- **The Collector** is a separate program in the middle: **receivers** take data in, **processors** change it (batch, filter, scrub, sample), **exporters** send it on. **Pipelines** connect them per signal.
- **Semantic conventions** are the agreed attribute names (`service.name`, `http.request.method`, ...), so tools understand data from any service. They change over time: here, server spans are now named `{method} {route}`.

## 2. Why a collector at all?

The services could export straight to a vendor. The collector earns its place by doing things once, centrally:
- **Scrubbing** sensitive data before it leaves your network.
- **Sampling** decisions that need the whole trace (tail sampling).
- **Batching, retries and memory limits**, so a slow backend doesn't hurt the services.
- **Routing** to several backends, or changing vendors without redeploying services.

The **builder** (`ocb`) compiles a collector with only the components you list: a smaller binary and a smaller attack surface than the all-in-one distribution.

## 3. Head vs tail sampling

| | Head sampling | Tail sampling |
|---|---|---|
| Where | in each service's SDK | in the collector |
| When it decides | when the trace *starts* | after the trace *ends* |
| Knows about errors and latency? | no | yes |
| Cost | cheap: unsampled spans are never even created | the collector holds every trace in memory for `decision_wait` |
| Scaling catch | none | every span of a trace must reach the **same** collector instance (a load-balancing tier routing by trace ID) |

Rule of thumb: head-sample for volume when every trace is equally interesting; tail-sample when the rare ones (errors, slow) matter most, which is usually.

## 4. Pieces used here

| Piece | Where | Idea |
|---|---|---|
| `otelhttp` handler and transport | `cmd/checkout`, `cmd/loadgen` | server/client spans and context propagation for free |
| `ParentBased(TraceIDRatioBased)` | `internal/telemetry` | head sampling that follows the caller's decision |
| `otelslog` bridge | `internal/telemetry` | `slog` lines become OTel logs with trace IDs |
| exemplars | histogram `checkout.duration` | a metric bucket points at a real trace |
| `processor.NewFactory`, `processorhelper` | `scrubprocessor/factory.go` | how a collector component is built |
| `pdata` (`ptrace`, `plog`, `pcommon`) | `scrubprocessor/scrub.go` | the collector's in-memory data model |
| `MutatesData` capability | `scrubprocessor/factory.go` | tells the collector to copy data for this processor when pipelines fan out |
| collector internal metrics | `scrub_redactions` | components report on themselves |
| `tail_sampling` processor | `collector/tail.yaml` | keep-if-any-policy-matches |
| builder manifest | `collector/builder-config.yaml` | a custom distribution, including a local component |

## 5. Explain it without notes

Numbers to anchor your answers: head sampling kept 11 of 102 failed checkouts, tail sampling all 102 (and all 34 slow ones) with 76% less trace data; 0 of 10,306 leaked values reached the output; the processor went from 35 ms to 13.5 ms per 1,000 leaky spans.

1. Walk a request from `loadgen` through `checkout` to the collector and out to a file. Where does the trace ID travel?
2. Why scrub in the collector instead of fixing each service? Why do both anyway?
3. How does the processor avoid masking order numbers that look like card numbers?
4. What made the processor 2.6× faster, and why is that the right kind of optimisation?
5. Head vs tail sampling: what does each keep of the errors, and why?
6. What does `decision_wait` trade off? What happens to a span that arrives after the decision?
7. Why must all spans of a trace reach the same collector for tail sampling, and how do you do that with several collectors?
8. What's an exemplar, and how would you use one during an incident?
9. Why build a custom collector with the builder instead of using the contrib distribution?
10. What would you alert on in the collector's own metrics?

## 6. Your turn

1. Add a detector for Indian PAN or Aadhaar-like numbers, with a checksum where one exists, and tests.
2. Keep logs only for sampled traces: after tail sampling, drop log records whose trace wasn't kept. Where in the pipeline can that happen, and what does it cost?
3. Run two collectors behind a `loadbalancing` exporter routing by trace ID, and show tail sampling still keeps every error.
4. Export to a real backend (Jaeger or Grafana Tempo in a container) and find a slow trace from a histogram exemplar.
