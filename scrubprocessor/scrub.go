// Package scrubprocessor is an OpenTelemetry Collector processor that removes
// secrets and personal data from traces and logs before they are exported.
//
// Telemetry is copied to vendors, kept for weeks and read by many people, so
// a card number in a span attribute is a data leak. Instrumentation leaks
// like this all the time (a header recorded "for debugging", an error message
// that includes the input), so the collector is the place to catch it once,
// for every service.
package scrubprocessor

import (
	"context"
	"regexp"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type detector struct {
	maybe func(string) bool // a cheap test first: most strings skip the regexp entirely
	re    *regexp.Regexp
	valid func(string) bool // optional second check on a match
}

func digits(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			n++
		}
	}
	return n
}

func containsFold(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }

var detectors = map[string]detector{
	// 13-19 digits, optionally grouped by spaces or dashes; only masked if it
	// passes the Luhn checksum, so order numbers and timestamps survive.
	"card": {maybe: func(s string) bool { return digits(s) >= 13 },
		re: regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`), valid: luhn},
	"email": {maybe: func(s string) bool { return strings.IndexByte(s, '@') >= 0 },
		re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	"bearer": {maybe: func(s string) bool { return containsFold(s, "bearer") },
		re: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/]+=*`)},
	"jwt": {maybe: func(s string) bool { return strings.Contains(s, "eyJ") },
		re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)},
	"aws_key": {maybe: func(s string) bool { return strings.Contains(s, "AKIA") || strings.Contains(s, "ASIA") },
		re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
}

// Bearer runs before jwt so "Bearer eyJ..." is reported as one bearer token.
var detectorOrder = []string{"bearer", "jwt", "aws_key", "card", "email"}

func luhn(s string) bool {
	sum, n := 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == ' ' || c == '-' {
			continue
		}
		d := int(c - '0')
		if n%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		n++
	}
	return n >= 13 && sum%10 == 0
}

type scrubber struct {
	drop     map[string]bool
	allow    map[string]bool
	active   []string
	redacted metric.Int64Counter
	dropped  metric.Int64Counter
}

func newScrubber(cfg *Config, meter metric.Meter) (*scrubber, error) {
	s := &scrubber{drop: map[string]bool{}, allow: map[string]bool{}}
	for _, k := range cfg.DropKeys {
		s.drop[strings.ToLower(k)] = true
	}
	for _, k := range cfg.AllowKeys {
		s.allow[strings.ToLower(k)] = true
	}
	want := map[string]bool{}
	for _, d := range cfg.Detectors {
		want[strings.ToLower(d)] = true
	}
	for _, d := range detectorOrder {
		if want[d] {
			s.active = append(s.active, d)
		}
	}
	var err error
	if s.redacted, err = meter.Int64Counter("scrub_redactions",
		metric.WithDescription("Values masked by the scrub processor, by detector")); err != nil {
		return nil, err
	}
	if s.dropped, err = meter.Int64Counter("scrub_dropped_attributes",
		metric.WithDescription("Attributes removed by the scrub processor")); err != nil {
		return nil, err
	}
	return s, nil
}

// redact masks every detected value in s, returning the new string and the
// count per detector.
func (s *scrubber) redact(in string, counts map[string]int64) string {
	out := in
	for _, name := range s.active {
		d := detectors[name]
		if !d.maybe(out) {
			continue
		}
		out = d.re.ReplaceAllStringFunc(out, func(m string) string {
			if d.valid != nil && !d.valid(m) {
				return m
			}
			counts[name]++
			return "[REDACTED:" + name + "]"
		})
	}
	return out
}

// scrubMap drops banned keys and masks values, recursing into nested maps
// and slices.
func (s *scrubber) scrubMap(m pcommon.Map, counts map[string]int64) {
	m.RemoveIf(func(k string, _ pcommon.Value) bool {
		if s.drop[strings.ToLower(k)] {
			counts["_dropped"]++
			return true
		}
		return false
	})
	m.Range(func(k string, v pcommon.Value) bool {
		if !s.allow[strings.ToLower(k)] {
			s.scrubValue(v, counts)
		}
		return true
	})
}

func (s *scrubber) scrubValue(v pcommon.Value, counts map[string]int64) {
	switch v.Type() {
	case pcommon.ValueTypeStr:
		if out := s.redact(v.Str(), counts); out != v.Str() {
			v.SetStr(out)
		}
	case pcommon.ValueTypeMap:
		s.scrubMap(v.Map(), counts)
	case pcommon.ValueTypeSlice:
		sl := v.Slice()
		for i := 0; i < sl.Len(); i++ {
			s.scrubValue(sl.At(i), counts)
		}
	}
}

func (s *scrubber) record(ctx context.Context, signal string, counts map[string]int64) {
	for name, n := range counts {
		if name == "_dropped" {
			s.dropped.Add(ctx, n, metric.WithAttributes(attribute.String("signal", signal)))
			continue
		}
		s.redacted.Add(ctx, n, metric.WithAttributes(attribute.String("detector", name), attribute.String("signal", signal)))
	}
}

func (s *scrubber) processTraces(ctx context.Context, td ptrace.Traces) (ptrace.Traces, error) {
	counts := map[string]int64{}
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		s.scrubMap(rs.Resource().Attributes(), counts)
		sss := rs.ScopeSpans()
		for j := 0; j < sss.Len(); j++ {
			spans := sss.At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)
				s.scrubMap(span.Attributes(), counts)
				if msg := span.Status().Message(); msg != "" {
					span.Status().SetMessage(s.redact(msg, counts))
				}
				events := span.Events()
				for e := 0; e < events.Len(); e++ {
					s.scrubMap(events.At(e).Attributes(), counts)
				}
				links := span.Links()
				for l := 0; l < links.Len(); l++ {
					s.scrubMap(links.At(l).Attributes(), counts)
				}
			}
		}
	}
	s.record(ctx, "traces", counts)
	return td, nil
}

func (s *scrubber) processLogs(ctx context.Context, ld plog.Logs) (plog.Logs, error) {
	counts := map[string]int64{}
	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		s.scrubMap(rl.Resource().Attributes(), counts)
		sls := rl.ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			records := sls.At(j).LogRecords()
			for k := 0; k < records.Len(); k++ {
				r := records.At(k)
				s.scrubMap(r.Attributes(), counts)
				s.scrubValue(r.Body(), counts)
			}
		}
	}
	s.record(ctx, "logs", counts)
	return ld, nil
}
