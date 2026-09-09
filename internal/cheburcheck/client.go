package cheburcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/tmaxmax/go-sse"
)

type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

func New(baseURL string, client *http.Client) *Client {
	return &Client{baseURL: baseURL, http: client, timeout: 3 * time.Minute}
}

func (c *Client) Check(ctx context.Context, target string) (monitor.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	report := monitor.Report{Target: target}
	base := strings.TrimRight(c.baseURL, "/")
	res, err := c.get(ctx, base+"/api/v1/check?target="+url.QueryEscape(target), "application/json")
	if err != nil {
		return report, err
	}
	var check struct {
		ID string `json:"id"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&check)
	res.Body.Close()
	if err != nil {
		return report, fmt.Errorf("check response: %w", err)
	}
	if check.ID == "" {
		return report, fmt.Errorf("Cheburcheck returned no check id")
	}
	report.JobID = check.ID
	res, err = c.get(ctx, base+"/api/v1/probe/"+url.PathEscape(check.ID), "text/event-stream")
	if err != nil {
		return report, err
	}
	defer res.Body.Close()
	media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if media != "text/event-stream" {
		return report, fmt.Errorf("Cheburcheck returned non-SSE response")
	}
	return readReport(res.Body, report)
}

func readReport(body io.Reader, report monitor.Report) (monitor.Report, error) {
	seen := map[string]bool{}
	started := false
	for event, err := range sse.Read(io.LimitReader(body, 16<<20), &sse.ReadConfig{MaxEventSize: 1 << 20}) {
		if err != nil {
			return report, fmt.Errorf("SSE: %w", err)
		}
		switch event.Type {
		case "started":
			var data struct {
				ID     string `json:"id"`
				Online int    `json:"online_probes"`
				Target string `json:"target"`
			}
			if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
				return report, fmt.Errorf("invalid started event")
			}
			if started || data.ID != report.JobID || data.Target != report.Target || data.Online < 0 {
				return report, fmt.Errorf("unexpected started event")
			}
			started = true
			report.Online = data.Online
		case "result":
			var p monitor.Probe
			if err := json.Unmarshal([]byte(event.Data), &p); err != nil {
				return report, fmt.Errorf("invalid result event")
			}
			if !started || p.ID == "" || p.JobID != report.JobID || len(p.Verdicts) == 0 {
				return report, fmt.Errorf("invalid probe identity or verdicts")
			}
			if !seen[p.ID] {
				report.Probes = append(report.Probes, p)
				seen[p.ID] = true
			}
		case "done":
			var data struct {
				ID     string `json:"id"`
				Count  int    `json:"response_count"`
				Online int    `json:"online_probes"`
			}
			if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
				return report, fmt.Errorf("invalid done event")
			}
			if !started || data.ID != report.JobID || data.Count != len(report.Probes) || data.Online != report.Online {
				return report, fmt.Errorf("inconsistent done event")
			}
			report.Done = true
			return report, nil
		case "error":
			return report, fmt.Errorf("Cheburcheck SSE error event")
		}
	}
	return report, fmt.Errorf("SSE ended without done event")
}

func (c *Client) get(ctx context.Context, address, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "rw-chebur-monitor/1")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		delay := time.Duration(0)
		if n, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && n > 0 {
			delay = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(res.Header.Get("Retry-After")); err == nil {
			delay = max(time.Duration(0), time.Until(at))
		}
		if res.StatusCode == http.StatusTooManyRequests {
			return nil, &monitor.RateLimitError{After: delay}
		}
		return nil, fmt.Errorf("Cheburcheck HTTP %d", res.StatusCode)
	}
	return res, nil
}
