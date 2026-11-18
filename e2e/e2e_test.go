// Package e2e runs the whole pipeline as real processes: the custom
// collector, the checkout service and the load generator, once per scenario
// (raw, head sampling, tail sampling), and compares what the collector wrote.
//
//	go test ./e2e -v      (needs collector/_build/otelcol-pipeline, from the builder)
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const requests = 2000

var root = filepath.Join("..")

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
	}
	t.Fatalf("%s never answered", url)
}

func start(t *testing.T, cmd *exec.Cmd, log string) {
	t.Helper()
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait(); f.Close() })
}

type sent struct {
	Status int   `json:"status"`
	Ms     int64 `json:"ms"`
}

type result struct {
	Scenario         string
	TracesKept       int
	SpansKept        int
	TraceBytes       int64
	ErrorsSent       int
	ErrorsKept       int
	SlowSent         int
	SlowKept         int
	NormalSent       int
	NormalKept       int
	Leaks            map[string]int
	LogRecords       int
	ScrubRedactions  string
	CollectorMetrics string
}

var leakPatterns = map[string]*regexp.Regexp{
	"card number":     regexp.MustCompile(`4111111111111111|5500005555555559|4012888888881881|378282246310005`),
	"email":           regexp.MustCompile(`user\d+@example\.com`),
	"bearer/jwt":      regexp.MustCompile(`Bearer eyJ|eyJhbGciOiJIUzI1NiJ9`),
	"auth header key": regexp.MustCompile(`http\.request\.header\.authorization`),
}

func run(t *testing.T, bins map[string]string, scenario, headRatio string) result {
	out := filepath.Join(t.TempDir(), scenario)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	otlp, metrics, svc := freePort(t), freePort(t), freePort(t)
	env := append(os.Environ(), fmt.Sprintf("OTLP_HTTP_PORT=%d", otlp), "OUT_DIR="+filepath.ToSlash(out),
		fmt.Sprintf("COLLECTOR_METRICS_PORT=%d", metrics))
	col := exec.Command(bins["collector"],
		"--config=file:"+filepath.Join(root, "collector", "common.yaml"),
		"--config=file:"+filepath.Join(root, "collector", scenario+".yaml"))
	col.Env = env
	start(t, col, filepath.Join(out, "collector.log"))
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", metrics))

	appEnv := append(os.Environ(), fmt.Sprintf("OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:%d", otlp),
		"HEAD_SAMPLE_RATIO="+headRatio, "LOADGEN_TRACES="+filepath.Join(out, "sent.json"))
	co := exec.Command(bins["checkout"], "-listen", fmt.Sprintf("127.0.0.1:%d", svc))
	co.Env = appEnv
	start(t, co, filepath.Join(out, "checkout.log"))
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", svc))

	lg := exec.Command(bins["loadgen"], "-url", fmt.Sprintf("http://127.0.0.1:%d/checkout", svc),
		"-n", fmt.Sprint(requests), "-c", "8")
	lg.Env = appEnv
	if b, err := lg.CombinedOutput(); err != nil {
		t.Fatalf("loadgen: %v\n%s", err, b)
	}
	// Tail sampling waits 2 s for a trace to complete; batches and file
	// writes flush within a second. Give everything time to land.
	time.Sleep(6 * time.Second)

	r := result{Scenario: scenario, Leaks: map[string]int{}}
	r.CollectorMetrics = scrape(t, metrics)
	for _, line := range strings.Split(r.CollectorMetrics, "\n") {
		if strings.HasPrefix(line, "scrub_redactions") || strings.HasPrefix(line, "otelcol_scrub_redactions") {
			r.ScrubRedactions += line + "\n"
		}
	}

	var sentTraces map[string]sent
	b, err := os.ReadFile(filepath.Join(out, "sent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &sentTraces); err != nil {
		t.Fatal(err)
	}
	kept := readTraces(t, filepath.Join(out, "traces.jsonl"), &r)
	for id, s := range sentTraces {
		_, ok := kept[id]
		switch {
		case s.Status != http.StatusOK:
			r.ErrorsSent++
			if ok {
				r.ErrorsKept++
			}
		case s.Ms >= 500:
			r.SlowSent++
			if ok {
				r.SlowKept++
			}
		default:
			r.NormalSent++
			if ok {
				r.NormalKept++
			}
		}
	}
	for _, f := range []string{"traces.jsonl", "logs.jsonl", "metrics.jsonl"} {
		data, _ := os.ReadFile(filepath.Join(out, f))
		for name, re := range leakPatterns {
			r.Leaks[name] += len(re.FindAll(data, -1))
		}
		if f == "logs.jsonl" {
			r.LogRecords = strings.Count(string(data), `"timeUnixNano"`)
		}
	}
	return r
}

func scrape(t *testing.T, port int) string {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// readTraces parses the file exporter's OTLP JSON lines into trace IDs.
func readTraces(t *testing.T, path string, r *result) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	r.TraceBytes = st.Size()
	type span struct {
		TraceID string `json:"traceId"`
	}
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []span `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	ids := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					ids[strings.ToLower(s.TraceID)] = true
					r.SpansKept++
				}
			}
		}
	}
	r.TracesKept = len(ids)
	return ids
}

func build(t *testing.T) map[string]string {
	bin := t.TempDir()
	bins := map[string]string{"collector": filepath.Join(root, "collector", "_build", exe("otelcol-pipeline"))}
	if _, err := os.Stat(bins["collector"]); err != nil {
		t.Fatalf("collector binary missing; build it first: go tool builder --config collector/builder-config.yaml")
	}
	for _, name := range []string{"checkout", "loadgen"} {
		bins[name] = filepath.Join(bin, exe(name))
		cmd := exec.Command("go", "build", "-o", bins[name], "./cmd/"+name)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, b)
		}
	}
	return bins
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func TestPipeline(t *testing.T) {
	bins := build(t)
	raw := run(t, bins, "raw", "1")
	head := run(t, bins, "head", "0.1")
	tail := run(t, bins, "tail", "1")

	t.Logf("%-5s %7s %7s %9s %13s %11s %13s %8s %v", "", "traces", "spans", "KB", "errors kept", "slow kept", "normal kept", "logs", "leaks")
	for _, r := range []result{raw, head, tail} {
		t.Logf("%-5s %7d %7d %9d %6d/%-6d %5d/%-5d %6d/%-6d %8d %v", r.Scenario, r.TracesKept, r.SpansKept, r.TraceBytes/1024,
			r.ErrorsKept, r.ErrorsSent, r.SlowKept, r.SlowSent, r.NormalKept, r.NormalSent, r.LogRecords, r.Leaks)
	}
	t.Logf("tail scenario scrub metrics:\n%s", tail.ScrubRedactions)
	writeSummary(t, raw, head, tail)

	// raw: everything kept, and the leaks are really there.
	if raw.TracesKept != requests {
		t.Errorf("raw kept %d traces, want all %d", raw.TracesKept, requests)
	}
	if raw.Leaks["card number"] == 0 || raw.Leaks["email"] == 0 || raw.Leaks["bearer/jwt"] == 0 {
		t.Errorf("the raw scenario should show the leaks the scrubber removes: %v", raw.Leaks)
	}
	// Both scrubbed scenarios: nothing sensitive in any output file.
	for _, r := range []result{head, tail} {
		for name, n := range r.Leaks {
			if n != 0 {
				t.Errorf("%s: %d %s leak(s) reached the exporter", r.Scenario, n, name)
			}
		}
	}
	// Tail sampling keeps every error and slow trace, and about 1 in 10 of the rest.
	if tail.ErrorsKept != tail.ErrorsSent || tail.SlowKept != tail.SlowSent {
		t.Errorf("tail sampling lost traces it must keep: errors %d/%d, slow %d/%d",
			tail.ErrorsKept, tail.ErrorsSent, tail.SlowKept, tail.SlowSent)
	}
	if p := pct(tail.NormalKept, tail.NormalSent); p < 5 || p > 15 {
		t.Errorf("tail kept %.1f%% of normal traces, want about 10%%", p)
	}
	// Head sampling decides before the outcome is known: it keeps about
	// 1 in 10 of everything, errors included.
	if p := pct(head.ErrorsKept, head.ErrorsSent); p > 30 {
		t.Errorf("head sampling kept %.1f%% of error traces; expected roughly 10%%", p)
	}
}

func writeSummary(t *testing.T, rs ...result) {
	b, _ := json.MarshalIndent(rs, "", " ")
	_ = os.MkdirAll(filepath.Join(root, "results"), 0o755)
	if err := os.WriteFile(filepath.Join(root, "results", "pipeline.json"), b, 0o644); err != nil {
		t.Log(err)
	}
}
