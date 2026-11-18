// Package checkout is a small instrumented service: one endpoint whose work
// shows up as a trace (three child spans), metrics and logs.
//
// It misbehaves on purpose, the way real services do by accident:
//   - about 5% of payments fail and about 2% are slow, so there is something
//     for sampling to keep;
//   - it records a customer's email and card number in span attributes, puts
//     an Authorization header on a span, and logs the card number on failure,
//     so there is something for the collector to scrub.
//
// Randomness comes from a seeded source, so a test run is repeatable.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const name = "github.com/builtbyshailendra/otel-pipeline/checkout"

// Behaviour sets the failure and slowness rates and how long "slow" is.
type Behaviour struct {
	ErrorRate float64
	SlowRate  float64
	Slow      time.Duration
	Seed      uint64
}

// Service handles POST /checkout.
type Service struct {
	b        Behaviour
	mu       sync.Mutex
	rng      *rand.Rand
	tracer   trace.Tracer
	requests metric.Int64Counter
	duration metric.Float64Histogram
}

// New builds the service from the global OTel providers.
func New(b Behaviour) (*Service, error) {
	meter := otel.Meter(name)
	requests, err := meter.Int64Counter("checkout.requests", metric.WithDescription("Checkouts by outcome"))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram("checkout.duration", metric.WithUnit("ms"),
		metric.WithDescription("Checkout time; exemplars link slow buckets to example traces"))
	if err != nil {
		return nil, err
	}
	return &Service{b: b, rng: rand.New(rand.NewPCG(b.Seed, 7)), tracer: otel.Tracer(name),
		requests: requests, duration: duration}, nil
}

func (s *Service) roll() (fail, slow bool, card int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rng.Float64() < s.b.ErrorRate, s.rng.Float64() < s.b.SlowRate, s.rng.IntN(4)
}

// Test card numbers (they pass the Luhn check, like real ones).
var cards = []string{"4111111111111111", "5500005555555559", "4012888888881881", "378282246310005"}

// ServeHTTP is the endpoint. Wrap it in otelhttp so the request gets a
// server span and incoming trace context is honoured.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()
	fail, slow, c := s.roll()
	user := r.URL.Query().Get("user")
	if user == "" {
		user = "guest"
	}
	email := user + "@example.com"
	card := cards[c]

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("user.email", email),  // personal data: the collector must scrub it
		attribute.String("payment.card", card), // a card number: the collector must scrub it
		// A header copied onto a span by over-eager instrumentation.
		attribute.String("http.request.header.authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2lnbmF0dXJl"),
	)
	slog.InfoContext(ctx, "checkout started", "user", email)

	err := s.work(ctx, fail, slow, card)
	outcome := "ok"
	if err != nil {
		outcome = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		slog.ErrorContext(ctx, fmt.Sprintf("payment declined for card %s: %v", card, err), "user", email)
		http.Error(w, "payment failed", http.StatusPaymentRequired)
	} else {
		fmt.Fprintln(w, `{"status":"paid"}`)
	}
	ms := float64(time.Since(start).Microseconds()) / 1000
	s.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	s.duration.Record(ctx, ms, metric.WithAttributes(attribute.String("outcome", outcome)))
}

var errDeclined = errors.New("card declined by issuer")

func (s *Service) work(ctx context.Context, fail, slow bool, card string) error {
	ctx, cart := s.tracer.Start(ctx, "load cart")
	time.Sleep(2 * time.Millisecond)
	cart.SetAttributes(attribute.Int("cart.items", 3))
	cart.End()

	ctx, pay := s.tracer.Start(ctx, "charge card", trace.WithAttributes(attribute.String("payment.provider", "fakepay")))
	if slow {
		time.Sleep(s.b.Slow)
		pay.AddEvent("provider retried", trace.WithAttributes(attribute.Int("attempt", 2)))
	} else {
		time.Sleep(3 * time.Millisecond)
	}
	if fail {
		pay.RecordError(errDeclined, trace.WithAttributes(attribute.String("payment.card", card)))
		pay.SetStatus(codes.Error, errDeclined.Error())
		pay.End()
		return errDeclined
	}
	pay.End()

	_, receipt := s.tracer.Start(ctx, "send receipt")
	time.Sleep(time.Millisecond)
	receipt.End()
	return nil
}
