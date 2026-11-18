# otel-pipeline

Telemetry you can trust and afford: an instrumented service, your own OpenTelemetry Collector, a processor that strips secrets and personal data before anything leaves, and tail-based sampling that keeps every error while dropping most of the rest.

Status: built step by step, locally. Not published.

## Build log

### Step 1: an instrumented service that leaks, on purpose

- `internal/telemetry`: the OpenTelemetry Go SDK (v1.47) for all three signals, exported over OTLP/HTTP: traces (batching, `ParentBased(TraceIDRatioBased)` head sampling from `HEAD_SAMPLE_RATIO`), metrics (every second), logs (Go's `slog` bridged to OTel, so every log line carries its trace and span ID). W3C trace context propagation.
- `internal/checkout`: `POST /checkout` with three child spans (load cart, charge card, send receipt), a request counter and a latency histogram. About 5% of payments fail and 2% are slow. It also leaks the way real services do: the customer's email and card number in span attributes, an `Authorization` header copied onto a span, and the card number in an error log.
- `cmd/loadgen`: an instrumented client, so every trace spans two services (`loadgen` → `checkout`).

Test (in-memory exporters): 200 checkouts produce 200 complete traces; every HTTP failure is a trace marked as an error with no receipt span; slow ones carry a "provider retried" event; the latency histogram carries exemplars (8 here) linking buckets to example traces.

What broke: the test looked for a server span named `POST /checkout`; the current HTTP conventions name it `{method} {route}`, and just `POST` when no route is known (the test has no router). The test now finds the server span by its kind.
