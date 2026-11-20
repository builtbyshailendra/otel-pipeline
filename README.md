# otel-pipeline

[![ci](https://github.com/builtbyshailendra/otel-pipeline/actions/workflows/ci.yml/badge.svg)](https://github.com/builtbyshailendra/otel-pipeline/actions/workflows/ci.yml)

Telemetry you can trust and afford. An instrumented Go service sends traces, metrics and logs to **its own OpenTelemetry Collector**, built with the official builder and including a **processor I wrote that strips secrets and personal data**. **Tail-based sampling** in the collector keeps every failed or slow request while dropping most of the rest.

Go 1.27 · OpenTelemetry Go SDK 1.47 · Collector v0.165 (core v1.71) · contrib tail sampling and file exporter. Personal learning project, built pairing with Claude Code. Everything ran on this laptop as real processes; no observability backend is involved (the collector writes files).

## Results (2,000 checkouts per scenario)

| Scenario | Errors kept | Slow kept | Trace data | Secrets reaching the output |
|---|---|---|---|---|
| raw (no scrubbing, no sampling) | 102 / 102 | 34 / 34 | 6,000 KB | 10,306 |
| head sampling 10% + scrub | **11 / 102** | **2 / 34** | 565 KB | 0 |
| tail sampling + scrub | **102 / 102** | **34 / 34** | 1,424 KB (−76%) | 0 |

Other numbers:
- The scrub processor handles 1,000 all-leaky spans in 13.5 ms, down from 35 ms before cheap pre-checks.
- The custom collector has seven components in a 45.7 MB binary.

## How it fits together

```text
loadgen ──traceparent──► checkout ──┐   (OTel Go SDK: traces, metrics, logs; OTLP/HTTP)
   └────────────────────────────────┴──► otelcol-pipeline (built with ocb)
                                            otlp receiver
                                            → memory_limiter → scrub → tail_sampling → batch
                                            → file exporter (traces.jsonl, logs.jsonl, metrics.jsonl)
                                            own metrics on :COLLECTOR_METRICS_PORT (scrub_redactions, ...)
```

## Try it

```bash
go tool builder --config collector/builder-config.yaml   # builds collector/_build/otelcol-pipeline
go test ./... && (cd scrubprocessor && go test ./...)     # unit tests
go test ./e2e -v                                          # the three scenarios, ~40 s
```

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

### Step 3: a custom collector, and the experiment

**The collector.** `collector/builder-config.yaml` lists exactly seven components (OTLP receiver; memory limiter, batch, tail sampling and this repo's `scrub` processors; debug and file exporters), and the OpenTelemetry Collector Builder (`go tool builder`, v0.165.0, pinned as a Go tool) generates and compiles them into one binary: 45.7 MB, about 4.5 minutes to generate, fetch and compile here. `collector/*.yaml` are the configs; the collector merges `common.yaml` with one scenario file.

**The experiment** (`e2e/e2e_test.go`): the real collector, service and load generator as separate processes, 2,000 checkouts per scenario, then the collector's output files are parsed and checked against what the load generator sent:

| Scenario | Traces kept | Spans | Trace data | Errors kept | Slow (≥ 500 ms) kept | Others kept | Secrets in the output |
|---|---|---|---|---|---|---|---|
| **raw**: no scrubbing, no sampling | 2,000 | 11,898 | 6,000 KB | 102 / 102 | 34 / 34 | 1,864 / 1,864 | **10,306**: 2,204 card numbers, 4,102 emails, 2,000 bearer JWTs, 2,000 auth-header keys |
| **head**: scrub + 10% head sampling in the services | 188 | 1,117 | 565 KB | **11 / 102** | **2 / 34** | 175 / 1,864 | 0 |
| **tail**: scrub + tail sampling in the collector | 330 | 1,878 | 1,424 KB | **102 / 102** | **34 / 34** | 194 / 1,864 (10.4%) | 0 |

What it shows:
- **Head sampling throws away what you need most.** It decides when a trace starts, so it kept about 1 in 10 of everything: 11 of the 102 failed checkouts and 2 of the 34 slow ones.
- **Tail sampling keeps every error and every slow trace** and still cuts trace volume by 76% (6,000 → 1,424 KB). It's bigger than head sampling's output because the traces it keeps on purpose are the long ones, with exception events.
- **Nothing sensitive survived scrubbing**: 0 of 10,306. The processor's own metrics account for it: 2,102 card numbers in traces (attributes and exception events), 102 in logs, 4,102 emails; the 2,000 Authorization headers were dropped by key.
- **Sampling traces doesn't reduce logs**: all 2,103 log records arrived in every scenario. Log volume needs its own policy (levels, or keeping logs only for sampled traces).

What broke:
1. **The services wouldn't start**: `conflicting Schema URL: .../1.43.0 and .../1.37.0`. The SDK's default resource uses semantic conventions 1.43.0; my code imported the 1.37.0 package, and merging resources with different schema URLs is an error. The unit test hadn't caught it because it never ran `telemetry.Setup`; the end-to-end run did. Fixed by importing the matching version; in 1.43, `deployment.environment.name` is an enumerated key, so the helper function changed too.

### Step 4: docs

This README's summary, `LEARN.md`, and an interview brief. Where it stands: unit tests, processor tests and the end-to-end experiment pass, locally and in CI (badge above; CI builds the collector too). Published 20 Nov 2026.
