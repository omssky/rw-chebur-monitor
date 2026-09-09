package monitor_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/omssky/rw-chebur-monitor/internal/sqlite"
	"github.com/stretchr/testify/require"
)

type offlinePanel struct{}

func (offlinePanel) Discover(context.Context) ([]monitor.Target, []string, error) {
	return nil, nil, errors.New("panel offline")
}

type checkFunc func(context.Context, string) (monitor.Report, error)

func (f checkFunc) Check(ctx context.Context, address string) (monitor.Report, error) {
	return f(ctx, address)
}

type silentSender struct{}

func (silentSender) Send(context.Context, string) error { return nil }

func TestServiceRetainsInventoryAndAppliesGlobalCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "monitor.db"))
		require.NoError(t, err)
		defer store.Close()
		state := monitor.NewState()
		for _, address := range []string{"a.example.com", "b.example.com"} {
			state.Targets[address] = &monitor.TargetState{
				Target: monitor.Target{Address: address}, Incidents: make(map[string]*monitor.Incident),
			}
		}
		require.NoError(t, store.Save(t.Context(), state, nil, time.Now()))
		calls := 0
		service := monitor.Service{
			Discovery: offlinePanel{}, Sender: silentSender{}, Store: store,
			Checker: checkFunc(func(_ context.Context, address string) (monitor.Report, error) {
				calls++
				return monitor.Report{Target: address}, &monitor.RateLimitError{After: 90 * time.Second}
			}),
			Policy: monitor.Policy{Interval: 30 * time.Minute, ConfirmDelay: 3 * time.Minute},
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- service.Run(ctx) }()

		// Advance two scheduler ticks without waiting in real time.
		time.Sleep(2 * time.Second)
		synctest.Wait()
		cancel()
		require.NoError(t, <-done)
		loaded, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Len(t, loaded.Targets, 2, "saved inventory lost")
		require.Equal(t, 1, calls, "rate limit ignored")
		require.Equal(t, 89*time.Second, time.Until(loaded.NextProbe))
	})
}

func TestServiceRejectsRepeatedJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "monitor.db"))
		require.NoError(t, err)
		defer store.Close()
		state := monitor.NewState()
		state.Targets["node.example.com"] = &monitor.TargetState{
			Target: monitor.Target{Address: "node.example.com"}, LastJob: "old-job",
			Incidents: map[string]*monitor.Incident{
				"a|AS123|region": {FirstSeen: time.Now().Add(-3 * time.Minute), BadCount: 1},
			},
		}
		require.NoError(t, store.Save(t.Context(), state, nil, time.Now()))
		calls := 0
		service := monitor.Service{
			Discovery: offlinePanel{}, Sender: silentSender{}, Store: store,
			Checker: checkFunc(func(_ context.Context, address string) (monitor.Report, error) {
				calls++
				return monitor.Report{
					JobID: "old-job", Target: address, Done: true,
					Probes: []monitor.Probe{{ID: "a", ASN: "AS123", Region: "region", Verdicts: []string{"tspu_block"}}},
				}, nil
			}),
			Policy: monitor.Policy{Interval: 30 * time.Minute, ConfirmDelay: 3 * time.Minute},
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- service.Run(ctx) }()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		cancel()
		require.NoError(t, <-done)

		loaded, err := store.Load(t.Context())
		require.NoError(t, err)
		target := loaded.Targets["node.example.com"]
		require.Equal(t, 1, calls)
		require.Equal(t, 1, target.Failures, "repeated job must use the normal error backoff")
		require.Empty(t, target.Incidents, "repeated result must not confirm a block")
		require.Equal(t, 59*time.Second, time.Until(target.NextCheck))
	})
}
