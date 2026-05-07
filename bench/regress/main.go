// Command regress compares the latest bench result against a committed
// baseline and fails if any of the percentile metrics has regressed past
// the configured tolerance.
//
// Tolerances are looser at smaller N because percentile estimates from a
// 10-session smoke are noisier than the 100-session full run. The CI
// smoke uses the smaller-N tolerance; the human-driven 100-session run
// uses the tighter one.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

type result struct {
	N            int           `json:"n"`
	Successes    int           `json:"successes"`
	Failures     int           `json:"failures"`
	WallClock    time.Duration `json:"wall_clock_ns"`
	ProvisionP50 time.Duration `json:"provision_p50_ns"`
	ProvisionP95 time.Duration `json:"provision_p95_ns"`
	ProvisionP99 time.Duration `json:"provision_p99_ns"`
	FirstByteP50 time.Duration `json:"first_byte_p50_ns"`
	FirstByteP95 time.Duration `json:"first_byte_p95_ns"`
	FirstByteP99 time.Duration `json:"first_byte_p99_ns"`
}

func main() {
	currentPath := flag.String("current", "", "current bench result JSON")
	baselinePath := flag.String("baseline", "bench/baseline.json", "baseline JSON")
	tolerance := flag.Float64("tolerance", 2.0, "max allowed multiplier vs baseline (e.g. 2.0 = 100% regression)")
	flag.Parse()

	if *currentPath == "" {
		fail("--current is required")
	}

	current, err := load(*currentPath)
	if err != nil {
		fail("read current: %v", err)
	}
	baseline, err := load(*baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "regress: no baseline at %s; treating current as new baseline (%s)\n",
			*baselinePath, current.ProvisionP95)
		os.Exit(0)
	}

	if current.Failures > 0 {
		fail("current run had %d failed sessions out of %d", current.Failures, current.N)
	}
	successPct := float64(current.Successes) / float64(current.N)
	if successPct < 0.95 {
		fail("current success rate %.1f%% below 95%% floor", successPct*100)
	}

	type metric struct {
		name             string
		current, baseAbs time.Duration
	}
	checks := []metric{
		{"provision_p50", current.ProvisionP50, baseline.ProvisionP50},
		{"provision_p95", current.ProvisionP95, baseline.ProvisionP95},
		{"provision_p99", current.ProvisionP99, baseline.ProvisionP99},
		{"first_byte_p95", current.FirstByteP95, baseline.FirstByteP95},
	}

	regressions := 0
	for _, c := range checks {
		if c.baseAbs == 0 {
			continue
		}
		ratio := float64(c.current) / float64(c.baseAbs)
		marker := "OK"
		if ratio > *tolerance {
			regressions++
			marker = "REGRESSION"
		}
		fmt.Printf("regress: %-15s current=%-15s baseline=%-15s ratio=%.2fx tolerance=%.2fx [%s]\n",
			c.name, c.current, c.baseAbs, ratio, *tolerance, marker)
	}
	if regressions > 0 {
		fail("%d metric(s) regressed past tolerance %.2fx", regressions, *tolerance)
	}
	fmt.Println("regress: all metrics within tolerance")
}

func load(p string) (result, error) {
	b, err := os.ReadFile(p) // #nosec G304 -- path is operator-provided
	if err != nil {
		return result{}, err
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		return result{}, err
	}
	return r, nil
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "regress: "+format+"\n", a...)
	os.Exit(1)
}
