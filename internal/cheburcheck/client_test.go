package cheburcheck

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/stretchr/testify/require"
)

func sseFixture(verdict string) string {
	return fmt.Sprintf(`event: started
data: {"id":"job","target":"node.example.com","online_probes":2}

: ping

event: result
data: {"job_id":"job","probe_id":"1","asn":"AS123","verdicts":[%q]}

event: done
data: {"id":"job","online_probes":2,"response_count":1}

`, verdict)
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

	client := New(server.URL, server.Client())
	report, err := client.Check(t.Context(), "node.example.com")
	require.NoError(t, err)
	require.True(t, report.Done)
	require.Len(t, report.Probes, 1)
	require.Equal(t, []string{"ok"}, report.Probes[0].Verdicts)
	require.Equal(t, []string{"/api/v1/check", "/api/v1/probe/job"}, requests)
}

func TestSSERejectsTruncationAndWrongJob(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":   strings.Split(sseFixture("ok"), "event: done")[0],
		"wrong job":   strings.Replace(sseFixture("ok"), `"job_id":"job"`, `"job_id":"other"`, 1),
		"wrong count": strings.Replace(sseFixture("ok"), `"response_count":1`, `"response_count":9`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readReport(strings.NewReader(body), monitor.Report{JobID: "job", Target: "node.example.com"})
			require.Error(t, err)
		})
	}
}

func TestRateLimitAndNoAutomaticRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := New(server.URL, server.Client())
	_, err := client.Check(t.Context(), "node.example.com")
	var rate *monitor.RateLimitError
	require.ErrorAs(t, err, &rate)
	require.Equal(t, 90*time.Second, rate.After)
	require.Equal(t, 1, calls)
}

func TestSSEDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/check" {
			fmt.Fprint(w, `{"id":"job"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	client := New(server.URL, server.Client())
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Check(ctx, "node.example.com")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
