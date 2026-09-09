package monitor_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/omssky/rw-chebur-monitor/internal/sqlite"
)

type offlinePanel struct{}

func (offlinePanel) Discover(context.Context) ([]monitor.Target, []string, error) {
	return nil, nil, errors.New("panel offline")
}

type limitedChecker struct {
	calls  atomic.Int32
	called chan struct{}
}

func (c *limitedChecker) Check(_ context.Context, address string) (monitor.Report, error) {
	if c.calls.Add(1) == 1 {
		close(c.called)
	}
	return monitor.Report{Target: address}, &monitor.RateLimitError{After: 90 * time.Second}
}

type silentSender struct{}

func (silentSender) Send(context.Context, string) error { return nil }

func TestServiceRetainsInventoryAndAppliesGlobalCooldown(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := monitor.NewState()
	for _, address := range []string{"a.example.com", "b.example.com"} {
		state.Targets[address] = &monitor.TargetState{
			Target: monitor.Target{Address: address}, Incidents: make(map[string]*monitor.Incident),
		}
	}
	if err := store.Save(context.Background(), state, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	checker := &limitedChecker{called: make(chan struct{})}
	service := monitor.Service{
		Discovery: offlinePanel{}, Checker: checker, Sender: silentSender{}, Store: store,
		Policy: monitor.Policy{Interval: 30 * time.Minute, ConfirmDelay: 3 * time.Minute},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	select {
	case <-checker.called:
	case <-time.After(5 * time.Second):
		t.Fatal("saved targets were not checked")
	}
	// Let another scheduler tick happen while the provider's cooldown is active.
	time.Sleep(1200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Targets) != 2 || checker.calls.Load() != 1 || time.Until(loaded.NextProbe) < 80*time.Second {
		t.Fatal("inventory lost or rate limit ignored", loaded, checker.calls.Load())
	}
}
