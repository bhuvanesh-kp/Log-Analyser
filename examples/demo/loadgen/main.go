// Load generator for the demo server.
//
// Drives the fake server through 5 phases so you can watch log_analyser react:
//
//	1. Warmup        — steady baseline traffic (~10 rps), builds baseline
//	2. Normal        — continued baseline, lets min_baseline_samples fill
//	3. Rate spike    — sudden 10× burst for a few seconds (rate_spike)
//	4. Error surge   — hammer /api/broken and /api/forbidden (error_surge)
//	5. Latency spike — hit /api/slow hard (latency_spike)
//	6. Silence       — stop sending traffic (silence, if -silence-secs <= analyser threshold)
//
// Run (after starting the server):
//
//	go run ./examples/demo/loadgen -target http://localhost:8080
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type phase struct {
	name     string
	duration time.Duration
	rps      int
	paths    []string
}

func main() {
	target := flag.String("target", "http://localhost:8080", "Server base URL")
	warmupSecs := flag.Int("warmup", 20, "Warmup phase seconds (baseline traffic)")
	spikeSecs := flag.Int("spike", 8, "Rate-spike phase seconds")
	errorSecs := flag.Int("errors", 10, "Error-surge phase seconds")
	latencySecs := flag.Int("latency", 10, "Latency-spike phase seconds")
	silenceSecs := flag.Int("silence", 20, "Final silence phase seconds")
	baselineRPS := flag.Int("baseline-rps", 10, "Requests/sec during baseline")
	spikeRPS := flag.Int("spike-rps", 120, "Requests/sec during spike")
	flag.Parse()

	normalPaths := []string{"/", "/api/users", "/api/orders", "/health"}
	errorPaths := []string{"/api/broken", "/api/forbidden"}
	slowPaths := []string{"/api/slow"}

	phases := []phase{
		{"warmup    ", time.Duration(*warmupSecs) * time.Second, *baselineRPS, normalPaths},
		{"spike     ", time.Duration(*spikeSecs) * time.Second, *spikeRPS, normalPaths},
		{"cooloff   ", 5 * time.Second, *baselineRPS, normalPaths},
		{"errors    ", time.Duration(*errorSecs) * time.Second, *baselineRPS * 3, errorPaths},
		{"cooloff   ", 5 * time.Second, *baselineRPS, normalPaths},
		{"latency   ", time.Duration(*latencySecs) * time.Second, *baselineRPS * 2, slowPaths},
		{"silence   ", time.Duration(*silenceSecs) * time.Second, 0, nil},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	var sent, ok, errs atomic.Int64

	// Stats printer
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var lastSent int64
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := sent.Load()
				fmt.Printf("  [loadgen] sent=%d (+%d/s) ok=%d errs=%d\n",
					now, now-lastSent, ok.Load(), errs.Load())
				lastSent = now
			}
		}
	}()

	for _, p := range phases {
		log.Printf(">>> phase=%s duration=%s rps=%d paths=%v", p.name, p.duration, p.rps, p.paths)
		runPhase(client, *target, p, &sent, &ok, &errs)
	}

	close(stop)
	wg.Wait()
	log.Printf("done. total sent=%d ok=%d errs=%d", sent.Load(), ok.Load(), errs.Load())
}

func runPhase(client *http.Client, base string, p phase, sent, ok, errs *atomic.Int64) {
	if p.rps == 0 || len(p.paths) == 0 {
		time.Sleep(p.duration)
		return
	}

	interval := time.Second / time.Duration(p.rps)
	deadline := time.Now().Add(p.duration)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var wg sync.WaitGroup
	for time.Now().Before(deadline) {
		<-ticker.C
		path := p.paths[rand.Intn(len(p.paths))]
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			sent.Add(1)
			resp, err := client.Get(url)
			if err != nil {
				errs.Add(1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 400 {
				ok.Add(1)
			} else {
				errs.Add(1)
			}
		}(base + path)
	}
	wg.Wait()
}
