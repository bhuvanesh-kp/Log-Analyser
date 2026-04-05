// Fake JSON-logging web server for log_analyser demos.
//
// Writes one JSON log line per request to -log (default: demo.log) in the
// exact format expected by log_analyser's `json` parser:
//
//	{"timestamp":"...","level":"info","host":"...","method":"GET",
//	 "path":"/api/users","status_code":200,"latency_ms":12.5}
//
// Run:
//
//	go run ./examples/demo/server -log demo.log -addr :8080
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type logLine struct {
	Timestamp  string  `json:"timestamp"`
	Level      string  `json:"level"`
	Host       string  `json:"host"`
	Method     string  `json:"method"`
	Path       string  `json:"path"`
	StatusCode int     `json:"status_code"`
	LatencyMs  float64 `json:"latency_ms"`
}

var (
	logFile *os.File
	logMu   sync.Mutex
)

func writeLog(l logLine) {
	logMu.Lock()
	defer logMu.Unlock()
	b, _ := json.Marshal(l)
	logFile.Write(b)
	logFile.Write([]byte("\n"))
}

// handler returns a handler that simulates the given status code and latency.
// If statusCode == 0, it returns 200. Latency is sampled uniformly in [minMs,maxMs].
func handler(statusCode int, minMs, maxMs float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Simulate work
		latencyMs := minMs + rand.Float64()*(maxMs-minMs)
		time.Sleep(time.Duration(latencyMs) * time.Millisecond)

		code := statusCode
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		fmt.Fprintln(w, "ok")

		level := "info"
		switch {
		case code >= 500:
			level = "error"
		case code >= 400:
			level = "warn"
		}

		host := r.RemoteAddr
		if i := strings.LastIndex(host, ":"); i > 0 {
			host = host[:i]
		}

		writeLog(logLine{
			Timestamp:  start.UTC().Format(time.RFC3339Nano),
			Level:      level,
			Host:       host,
			Method:     r.Method,
			Path:       r.URL.Path,
			StatusCode: code,
			LatencyMs:  float64(time.Since(start).Microseconds()) / 1000.0,
		})
	}
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	logPath := flag.String("log", "demo.log", "JSON log output file")
	flag.Parse()

	var err error
	logFile, err = os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("open log: %v", err)
	}
	defer logFile.Close()

	mux := http.NewServeMux()
	// Normal endpoints
	mux.Handle("/", handler(200, 2, 20))
	mux.Handle("/api/users", handler(200, 5, 30))
	mux.Handle("/api/orders", handler(200, 10, 50))
	mux.Handle("/health", handler(200, 1, 3))

	// Deliberately slow endpoint (good for latency-spike demos)
	mux.Handle("/api/slow", handler(200, 200, 800))

	// Deliberately broken endpoints (good for error-surge demos)
	mux.Handle("/api/broken", handler(500, 5, 15))
	mux.Handle("/api/forbidden", handler(403, 2, 10))

	log.Printf("fake server listening on %s, logging to %s", *addr, *logPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}
