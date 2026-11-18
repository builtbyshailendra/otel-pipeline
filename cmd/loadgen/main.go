// Command loadgen sends checkouts and is itself instrumented, so each trace
// spans two services: loadgen (client span) and checkout (server spans).
//
//	loadgen -url http://127.0.0.1:8099/checkout -n 2000 -c 8
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"github.com/builtbyshailendra/otel-pipeline/internal/telemetry"
)

type sent struct {
	Status int   `json:"status"`
	Ms     int64 `json:"ms"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8099/checkout", "checkout endpoint")
	n := flag.Int("n", 1000, "requests")
	c := flag.Int("c", 8, "concurrency")
	flag.Parse()

	ctx := context.Background()
	shutdown, err := telemetry.Setup(ctx, "loadgen")
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport), Timeout: 30 * time.Second}
	tracer := otel.Tracer("loadgen")

	var ok, failed atomic.Int64
	var traceIDs sync.Map // trace id -> what happened, so tests can check what the collector kept
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for range *c {
		wg.Go(func() {
			for i := range jobs {
				began := time.Now()
				rctx, span := tracer.Start(ctx, "buy")
				req, _ := http.NewRequestWithContext(rctx, http.MethodPost, fmt.Sprintf("%s?user=user%d", *url, i), nil)
				resp, err := client.Do(req)
				status := 0
				if err == nil {
					status = resp.StatusCode
					resp.Body.Close()
				}
				span.End()
				traceIDs.Store(span.SpanContext().TraceID().String(), sent{Status: status, Ms: time.Since(began).Milliseconds()})
				if status == http.StatusOK {
					ok.Add(1)
				} else {
					failed.Add(1)
				}
			}
		})
	}
	for i := range *n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	took := time.Since(start)

	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := shutdown(sctx); err != nil {
		log.Printf("telemetry shutdown: %v", err)
	}
	summary := map[string]any{"requests": *n, "ok": ok.Load(), "failed": failed.Load(), "seconds": took.Seconds()}
	if path := os.Getenv("LOADGEN_TRACES"); path != "" {
		m := map[string]sent{}
		traceIDs.Range(func(k, v any) bool { m[k.(string)] = v.(sent); return true })
		b, _ := json.Marshal(m)
		_ = os.WriteFile(path, b, 0o600)
	}
	b, _ := json.Marshal(summary)
	fmt.Println(string(b))
}
