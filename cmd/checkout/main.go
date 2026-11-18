// Command checkout runs the instrumented service.
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 checkout -listen 127.0.0.1:8099
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/builtbyshailendra/otel-pipeline/internal/checkout"
	"github.com/builtbyshailendra/otel-pipeline/internal/telemetry"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8099", "address")
	errorRate := flag.Float64("error-rate", 0.05, "share of checkouts that fail")
	slowRate := flag.Float64("slow-rate", 0.02, "share of checkouts that are slow")
	slow := flag.Duration("slow", 600*time.Millisecond, "how slow a slow checkout is")
	seed := flag.Uint64("seed", 42, "random seed")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	shutdown, err := telemetry.Setup(ctx, "checkout")
	if err != nil {
		log.Fatal(err)
	}
	svc, err := checkout.New(checkout.Behaviour{ErrorRate: *errorRate, SlowRate: *slowRate, Slow: *slow, Seed: *seed})
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /checkout", otelhttp.NewHandler(svc, "POST /checkout"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("checkout listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(fctx); err != nil { // flush everything still buffered
		log.Printf("telemetry shutdown: %v", err)
	}
}
