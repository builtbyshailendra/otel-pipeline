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

### Step 2: a collector processor that scrubs secrets and personal data

`scrubprocessor/` is an OpenTelemetry Collector component (its own Go module, built on the collector's `processor`, `processorhelper` and `pdata` packages, v1.71/v0.165):

- **Drop** attribute keys that must never leave (Authorization and cookie headers, passwords), matched case-insensitively.
- **Mask** values in any string, nested map or list: card numbers (13–19 digits, **only if they pass the Luhn checksum**, so order numbers and timestamps survive), emails, bearer tokens, JWTs, AWS access keys, each replaced with `[REDACTED:<kind>]`.
- Covers resource, span, span-event and span-link attributes, span status messages, log attributes, and log bodies (plain or structured).
- `allow_keys` exempts known-safe fields; an unknown detector name fails config validation at startup instead of silently scrubbing nothing.
- Counts what it does on the collector's own metrics (`scrub_redactions{detector,signal}`, `scrub_dropped_attributes`).
- Declares `MutatesData`, so the collector gives it its own copy when a pipeline fans out.

Tests use the collector's own helpers (`processortest`, `consumertest` sinks): 11 redaction cases including two Luhn-failing numbers that must survive, a full leaky trace (resource, nested map, status message, exception event, a mixed-case header key, an allow-listed ID), structured log bodies, and config validation.

**Performance.** Benchmark: a batch of 1,000 spans that *all* contain secrets (the worst case). First version: **35 ms** per batch (157,000 allocations): every string went through five regular expressions. Adding a cheap check before each one (no `@` → can't be an email; fewer than 13 digits → can't be a card) brought it to **13.5 ms** (61,000 allocations), 2.6× faster. Clean strings, the normal case, now skip the regular expressions entirely.
