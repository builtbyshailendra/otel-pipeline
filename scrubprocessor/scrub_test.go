package scrubprocessor

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.opentelemetry.io/otel/metric/noop"
)

func defaultScrubber(t testing.TB) *scrubber {
	t.Helper()
	s, err := newScrubber(createDefaultConfig(), noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRedact(t *testing.T) {
	s := defaultScrubber(t)
	cases := []struct{ in, want string }{
		{"card 4111111111111111 declined", "card [REDACTED:card] declined"},
		{"card 4111 1111 1111 1111", "card [REDACTED:card]"},
		{"card 5500-0055-5555-5559", "card [REDACTED:card]"},
		{"amex 378282246310005", "amex [REDACTED:card]"},
		{"order 4111111111111112 shipped", "order 4111111111111112 shipped"}, // fails Luhn: not a card
		{"ts 1763460000123", "ts 1763460000123"},                             // 13 digits, fails Luhn
		{"from asha.r@example.com", "from [REDACTED:email]"},
		{"Authorization: Bearer abc.def-123", "Authorization: [REDACTED:bearer]"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln", "token [REDACTED:jwt]"},
		{"key AKIAABCDEFGHIJKLMNOP leaked", "key [REDACTED:aws_key] leaked"},
		{"nothing to see here", "nothing to see here"},
	}
	for _, tc := range cases {
		counts := map[string]int64{}
		if got := s.redact(tc.in, counts); got != tc.want {
			t.Errorf("redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestConfigRejectsUnknownDetectors(t *testing.T) {
	cfg := createDefaultConfig()
	cfg.Detectors = append(cfg.Detectors, "ssn")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ssn") {
		t.Fatalf("Validate() = %v", err)
	}
}

// leakyTraces builds what the checkout service sends: secrets in resource,
// span, event and status.
func leakyTraces() ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	rs.Resource().Attributes().PutStr("deploy.by", "ops@example.com")
	span := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName("POST /checkout")
	a := span.Attributes()
	a.PutStr("user.email", "user7@example.com")
	a.PutStr("payment.card", "4111111111111111")
	a.PutStr("HTTP.Request.Header.Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln")
	a.PutStr("order.id", "4111111111111111") // allow-listed below: left alone
	a.PutInt("cart.items", 3)
	nested := a.PutEmptyMap("customer")
	nested.PutStr("contact", "call 4012888888881881 or mail x@y.io")
	span.Status().SetMessage("declined for 5500005555555559")
	ev := span.Events().AppendEmpty()
	ev.SetName("exception")
	ev.Attributes().PutStr("exception.message", "card 378282246310005 declined")
	return td
}

func TestProcessorScrubsTracesEndToEnd(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	cfg.AllowKeys = []string{"order.id"}
	sink := new(consumertest.TracesSink)
	p, err := f.CreateTraces(context.Background(), processortest.NewNopSettings(componentType), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ConsumeTraces(context.Background(), leakyTraces()); err != nil {
		t.Fatal(err)
	}
	out := sink.AllTraces()[0].ResourceSpans().At(0)
	span := out.ScopeSpans().At(0).Spans().At(0)
	attrs := span.Attributes().AsRaw()

	if _, ok := attrs["HTTP.Request.Header.Authorization"]; ok {
		t.Error("the authorization header survived (keys must match case-insensitively)")
	}
	want := map[string]any{"user.email": "[REDACTED:email]", "payment.card": "[REDACTED:card]",
		"order.id": "4111111111111111", "cart.items": int64(3)}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("%s = %v, want %v", k, attrs[k], v)
		}
	}
	if got := attrs["customer"].(map[string]any)["contact"]; got != "call [REDACTED:card] or mail [REDACTED:email]" {
		t.Errorf("nested value = %q", got)
	}
	if got := out.Resource().Attributes().AsRaw()["deploy.by"]; got != "[REDACTED:email]" {
		t.Errorf("resource attribute = %v", got)
	}
	if got := span.Status().Message(); got != "declined for [REDACTED:card]" {
		t.Errorf("status message = %q", got)
	}
	if got := span.Events().At(0).Attributes().AsRaw()["exception.message"]; got != "card [REDACTED:card] declined" {
		t.Errorf("event attribute = %v", got)
	}
}

func TestProcessorScrubsLogs(t *testing.T) {
	f := NewFactory()
	sink := new(consumertest.LogsSink)
	p, err := f.CreateLogs(context.Background(), processortest.NewNopSettings(componentType), f.CreateDefaultConfig(), sink)
	if err != nil {
		t.Fatal(err)
	}
	ld := plog.NewLogs()
	recs := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	r := recs.AppendEmpty()
	r.Body().SetStr("payment declined for card 4111111111111111: card declined by issuer")
	r.Attributes().PutStr("user", "user3@example.com")
	m := recs.AppendEmpty()
	body := m.Body().SetEmptyMap()
	body.PutStr("msg", "retry with key AKIAABCDEFGHIJKLMNOP")
	body.PutEmptySlice("emails").AppendEmpty().SetStr("a@b.co")
	if err := p.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}
	got := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
	if b := got.At(0).Body().Str(); b != "payment declined for card [REDACTED:card]: card declined by issuer" {
		t.Errorf("body = %q", b)
	}
	if u := got.At(0).Attributes().AsRaw()["user"]; u != "[REDACTED:email]" {
		t.Errorf("user = %v", u)
	}
	mb := got.At(1).Body().Map().AsRaw()
	if mb["msg"] != "retry with key [REDACTED:aws_key]" || mb["emails"].([]any)[0] != "[REDACTED:email]" {
		t.Errorf("structured body = %v", mb)
	}
}

func BenchmarkProcessTraces(b *testing.B) {
	s := defaultScrubber(b)
	template := leakyTraces()
	// A batch of 1,000 spans like the leaky one.
	td := ptrace.NewTraces()
	for range 1000 {
		template.ResourceSpans().At(0).CopyTo(td.ResourceSpans().AppendEmpty())
	}
	b.ReportAllocs()
	for b.Loop() {
		batch := ptrace.NewTraces()
		td.CopyTo(batch)
		if _, err := s.processTraces(context.Background(), batch); err != nil {
			b.Fatal(err)
		}
	}
}
