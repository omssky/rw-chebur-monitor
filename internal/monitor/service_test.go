package monitor_test

import (
	"context"
	"errors"
	"fmt"
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

func (silentSender) Send(context.Context, monitor.Notification) (int, error) { return 1, nil }

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

type sendFunc func(context.Context, monitor.Notification) (int, error)

func (f sendFunc) Send(ctx context.Context, n monitor.Notification) (int, error) {
	return f(ctx, n)
}

func TestIncidentCardLifecycleThroughPersistentQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "monitor.db"))
		require.NoError(t, err)
		defer store.Close()
		state := monitor.NewState()
		state.Targets["node.example.com"] = &monitor.TargetState{
			Target:    monitor.Target{Address: "node.example.com", Names: []string{"Node"}},
			Incidents: make(map[string]*monitor.Incident),
		}
		require.NoError(t, store.Save(t.Context(), state, nil, time.Now()))
		var sent []monitor.Notification
		calls := 0
		service := monitor.Service{
			Discovery: offlinePanel{}, Store: store,
			Checker: checkFunc(func(_ context.Context, address string) (monitor.Report, error) {
				calls++
				a, b := "ok", "ok"
				if calls <= 4 {
					a = "tspu_block"
				}
				if calls == 3 || calls == 4 {
					b = "tspu_block"
				}
				return monitor.Report{
					JobID: fmt.Sprintf("job-%d", calls), Target: address, Done: true, Online: 2,
					Probes: []monitor.Probe{
						{ID: "a", ASN: "AS1", Region: "A", Verdicts: []string{a}},
						{ID: "b", ASN: "AS2", Region: "B", Verdicts: []string{b}},
					},
				}, nil
			}),
			Sender: sendFunc(func(_ context.Context, n monitor.Notification) (int, error) {
				sent = append(sent, n)
				if n.MessageID != 0 && n.Event.Kind == monitor.EventCard {
					return n.MessageID, nil
				}
				return 100 + len(sent), nil
			}),
			Policy: monitor.Policy{Interval: time.Minute, ConfirmDelay: 15 * time.Second},
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- service.Run(ctx) }()
		time.Sleep(6 * time.Minute)
		synctest.Wait()
		cancel()
		require.NoError(t, <-done)

		require.NotEmpty(t, sent)
		require.Equal(t, monitor.EventCard, sent[0].Event.Kind)
		require.Zero(t, sent[0].MessageID)
		episodeID := sent[0].Event.EpisodeID
		var escalations, summaries, closedCards int
		for _, n := range sent[1:] {
			require.Equal(t, episodeID, n.Event.EpisodeID)
			require.Equal(t, 101, n.MessageID, "updates and replies must reference the original card")
			switch n.Event.Kind {
			case monitor.EventEscalation:
				escalations++
			case monitor.EventSummary:
				summaries++
				require.Equal(t, 1, closedCards, "closing reply must follow the final edit")
			case monitor.EventCard:
				if !n.Event.Card.ClosedAt.IsZero() {
					closedCards++
				}
			}
		}
		require.Equal(t, 1, escalations)
		require.Equal(t, 1, summaries)
		require.Equal(t, 1, closedCards)
		next, err := store.NextNotification(t.Context(), time.Now())
		require.NoError(t, err)
		require.Nil(t, next)
		loaded, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Empty(t, loaded.Targets["node.example.com"].Incidents)
	})
}

func TestServiceRecreatesDeletedIncidentCard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "monitor.db"))
		require.NoError(t, err)
		defer store.Close()
		state := monitor.NewState()
		event := monitor.Event{Kind: monitor.EventCard, EpisodeID: "incident", Card: monitor.Card{
			Target: monitor.Target{Address: "node.example.com"}, StartedAt: time.Now(),
		}}
		require.NoError(t, store.Save(t.Context(), state, []monitor.Event{event}, time.Now()))
		opening, err := store.NextNotification(t.Context(), time.Now())
		require.NoError(t, err)
		require.NotNil(t, opening)
		require.NoError(t, store.Sent(t.Context(), *opening, 100))
		require.NoError(t, store.Save(t.Context(), state, []monitor.Event{event}, time.Now()))
		var messageIDs []int
		service := monitor.Service{
			Discovery: offlinePanel{}, Store: store,
			Sender: sendFunc(func(_ context.Context, n monitor.Notification) (int, error) {
				messageIDs = append(messageIDs, n.MessageID)
				if n.MessageID == 100 {
					return 0, &monitor.MessageMissingError{Err: errors.New("message to edit not found")}
				}
				return 101, nil
			}),
			Policy: monitor.Policy{Interval: time.Minute},
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- service.Run(ctx) }()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		cancel()
		require.NoError(t, <-done)
		require.Equal(t, []int{100, 0}, messageIDs)
		next, err := store.NextNotification(t.Context(), time.Now())
		require.NoError(t, err)
		require.Nil(t, next)
		require.NoError(t, store.Save(t.Context(), state, []monitor.Event{event}, time.Now()))
		next, err = store.NextNotification(t.Context(), time.Now())
		require.NoError(t, err)
		require.Equal(t, 101, next.MessageID)
	})
}
