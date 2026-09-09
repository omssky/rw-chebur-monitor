package cheburcheck

import (
	"context"
	"errors"
	"fmt"
	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sseFixture(verdict string) string {
	return fmt.Sprintf("event: started\ndata: {\"id\":\"job\",\"target\":\"node.example.com\",\"online_probes\":2}\n\n: ping\n\nevent: result\ndata: {\"job_id\":\"job\",\"probe_id\":\"1\",\"asn\":\"AS123\",\"verdicts\":[%q]}\n\nevent: done\ndata: {\"id\":\"job\",\"online_probes\":2,\"response_count\":1}\n\n", verdict)
}

func TestCheburcheckHTTPAndSSE(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/check":
			if r.URL.Query().Get("target") != "node.example.com" {
				t.Error("wrong target")
			}
			fmt.Fprint(w, `{"id":"job","blocked":true}`)
		case "/api/v1/probe/job":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sseFixture("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := Client{server.URL, testHTTPClient(0), time.Second}
	r, err := c.Check(context.Background(), "node.example.com")
	if err != nil || !r.Done || len(r.Probes) != 1 || r.Probes[0].Verdicts[0] != "ok" {
		t.Fatalf("%+v %v", r, err)
	}
	if len(requests) != 2 {
		t.Fatal(requests)
	}
}

func TestSSERejectsTruncationAndWrongJob(t *testing.T) {
	for _, body := range []string{
		strings.Split(sseFixture("ok"), "event: done")[0],
		strings.Replace(sseFixture("ok"), `"job_id":"job"`, `"job_id":"other"`, 1),
		strings.Replace(sseFixture("ok"), `"response_count":1`, `"response_count":9`, 1),
	} {
		if _, err := readReport(strings.NewReader(body), monitor.Report{JobID: "job", Target: "node.example.com"}); err == nil {
			t.Fatal("invalid stream accepted")
		}
	}
}

func TestRateLimitAndNoAutomaticRetry(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(429)
	}))
	defer s.Close()
	c := Client{s.URL, testHTTPClient(0), time.Second}
	_, err := c.Check(context.Background(), "node.example.com")
	var rate *monitor.RateLimitError
	if !errors.As(err, &rate) || rate.After != 90*time.Second || calls != 1 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestSSEDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/check" {
			fmt.Fprint(w, `{"id":"job"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	c := Client{s.URL, testHTTPClient(0), 50 * time.Millisecond}
	_, err := c.Check(context.Background(), "node.example.com")
	if err == nil {
		t.Fatal("expected deadline")
	}
}

func testHTTPClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }
