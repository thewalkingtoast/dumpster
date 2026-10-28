// Command loadgen posts to one dumpster from many goroutines and reports
// throughput and latency, so "highly concurrent" is a measurement.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "base url")
	id := flag.String("id", "", "dumpster uuid")
	total := flag.Int("n", 2000, "requests to send")
	conc := flag.Int("c", 100, "concurrent workers")
	size := flag.Int("size", 512, "body size in bytes")
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -id is required")
		os.Exit(2)
	}

	target := *base + "/d/" + *id + "/load"
	payload := bytes.Repeat([]byte("x"), *size)
	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: *conc, MaxConnsPerHost: *conc},
		Timeout:   10 * time.Second,
	}

	var ok, failed int64
	var mu sync.Mutex
	latencies := make([]time.Duration, 0, *total)

	jobs := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < *conc; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				started := time.Now()
				res, err := client.Post(target, "application/octet-stream", bytes.NewReader(payload))
				if err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					atomic.AddInt64(&failed, 1)
					continue
				}
				atomic.AddInt64(&ok, 1)
				mu.Lock()
				latencies = append(latencies, time.Since(started))
				mu.Unlock()
			}
		}()
	}

	started := time.Now()
	for i := 0; i < *total; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(started)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(q float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)]
	}

	fmt.Printf("%d ok, %d failed in %s\n", ok, failed, elapsed.Round(time.Millisecond))
	fmt.Printf("%.0f req/s  p50=%s  p95=%s  p99=%s\n",
		float64(ok)/elapsed.Seconds(),
		pct(0.50).Round(time.Microsecond),
		pct(0.95).Round(time.Microsecond),
		pct(0.99).Round(time.Microsecond),
	)
	if failed > 0 {
		os.Exit(1)
	}
}
