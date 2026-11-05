package gen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// TestNowOffset is how long after generation Verify pins the clock (through
// X-Test-Now, honoured only when the service runs with TEST_MODE=true).
const TestNowOffset = time.Minute

// Result is the outcome of one case.
type Result struct {
	Case Case
	Got  Expected
	Err  error
}

// OK reports whether the service answered as expected.
func (r Result) OK() bool {
	return r.Err == nil && r.Got.Decision == r.Case.Expected.Decision && slices.Equal(r.Got.Rules, r.Case.Expected.Rules)
}

// WaitReady polls GET /health/ready until it returns 200 or ctx ends.
func WaitReady(ctx context.Context, client *http.Client, baseURL string) error {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health/ready", nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready: %w", baseURL, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Verify sends every case's decision request and compares the answers.
func Verify(ctx context.Context, client *http.Client, baseURL string, m *Manifest) []Result {
	testNow := m.GeneratedAt.Add(TestNowOffset).Format(time.RFC3339Nano)
	out := make([]Result, len(m.Cases))
	for i, c := range m.Cases {
		out[i] = Result{Case: c}
		out[i].Got, out[i].Err = decide(ctx, client, baseURL, testNow, c.Request)
	}
	return out
}

func decide(ctx context.Context, client *http.Client, baseURL, testNow string, dr DecisionRequest) (Expected, error) {
	body, err := json.Marshal(dr)
	if err != nil {
		return Expected{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/decisions", bytes.NewReader(body))
	if err != nil {
		return Expected{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Now", testNow)
	resp, err := client.Do(req)
	if err != nil {
		return Expected{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Expected{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Expected{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var d struct {
		Decision string `json:"decision"`
		Reasons  []struct {
			Rule string `json:"rule"`
		} `json:"reasons"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return Expected{}, fmt.Errorf("decode %q: %w", raw, err)
	}
	got := Expected{Decision: d.Decision, Rules: []string{}}
	for _, r := range d.Reasons {
		got.Rules = append(got.Rules, r.Rule)
	}
	return got, nil
}
