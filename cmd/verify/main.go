// Command verify sends the decision request of every planted scenario in a
// manifest and exits non-zero on any mismatch.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/devastion/anti-fraud-go/internal/gen"
)

func main() {
	var (
		manifest = flag.String("manifest", "manifest.json", "manifest written by generate")
		url      = flag.String("url", "http://localhost:8080", "service base URL")
		wait     = flag.Duration("wait", 10*time.Minute, "how long to wait for /health/ready")
	)
	flag.Parse()
	ok, err := run(*manifest, strings.TrimRight(*url, "/"), *wait)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

func run(manifestPath, url string, wait time.Duration) (bool, error) {
	m, err := gen.ReadManifest(manifestPath)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := gen.WaitReady(ctx, client, url); err != nil {
		return false, err
	}
	if age := time.Since(m.GeneratedAt); age > 9*time.Minute {
		fmt.Fprintf(os.Stderr, "warning: data generated %s ago; the R1 and R3 windows have expired "+
			"unless the service runs with TEST_MODE=true (then the clock is pinned to generation + %s)\n",
			age.Round(time.Second), gen.TestNowOffset)
	}

	results := gen.Verify(context.Background(), client, url, m)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tRULE\tKIND\tEXPECTED\tGOT\tRESULT")
	failed := 0
	for _, r := range results {
		status := "ok"
		got := format(r.Got)
		switch {
		case r.Err != nil:
			status, got = "ERROR", r.Err.Error()
			failed++
		case !r.OK():
			status = "MISMATCH"
			failed++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Case.Name, r.Case.Rule, r.Case.Kind, format(r.Case.Expected), got, status)
	}
	tw.Flush()
	fmt.Printf("\n%d/%d cases passed\n", len(results)-failed, len(results))
	return failed == 0, nil
}

func format(e gen.Expected) string {
	if len(e.Rules) == 0 {
		return e.Decision
	}
	return e.Decision + " " + strings.Join(e.Rules, ",")
}
