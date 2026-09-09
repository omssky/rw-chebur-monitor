package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/stretchr/testify/require"
)

func TestRestartAndOrderedDelivery(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "monitor.db")
	store, err := Open(path)
	require.NoError(t, err)
	defer store.Close()

	now := time.Now()
	state := monitor.NewState()
	state.Targets["node.example.com"] = &monitor.TargetState{
		Target:    monitor.Target{Address: "node.example.com"},
		Incidents: map[string]*monitor.Incident{"probe": {Open: true}},
	}
	require.NoError(t, store.Save(ctx, state, []string{"opening"}, now))
	require.NoError(t, store.Close())

	store, err = Open(path)
	require.NoError(t, err)
	defer store.Close()
	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	target := loaded.Targets["node.example.com"]
	require.NotNil(t, target, "target lost after restart")
	incident := target.Incidents["probe"]
	require.NotNil(t, incident, "incident lost after restart")
	require.True(t, incident.Open)

	first, err := store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, first, "notification lost after restart")
	require.NoError(t, store.Retry(ctx, first.ID, now.Add(time.Minute)))
	require.NoError(t, store.Save(ctx, loaded, []string{"recovery"}, now))

	next, err := store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.Nil(t, next, "recovery overtook opening")
	require.NoError(t, store.Sent(ctx, first.ID))

	next, err = store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "recovery", next.Body)
}

func TestTransactionRollbackAndExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	store, err := Open(path)
	require.NoError(t, err)
	defer store.Close()

	second, err := Open(path)
	if second != nil {
		defer second.Close()
	}
	require.Error(t, err, "second instance acquired database")

	ctx := t.Context()
	state := monitor.NewState()
	require.NoError(t, store.Save(ctx, state, nil, time.Now()))
	_, err = store.db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END;`)
	require.NoError(t, err)

	state.NextProbe = time.Now()
	require.Error(t, store.Save(ctx, state, []string{"alert"}, time.Now()))
	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.True(t, loaded.NextProbe.IsZero(), "state committed without alert")
}

func TestTelegramMessageLimit(t *testing.T) {
	message := strings.Repeat("🇫🇮 node ", 600)
	parts := splitMessage(message)
	require.Equal(t, message, strings.Join(parts, ""), "message corrupted")
	for _, part := range parts {
		require.LessOrEqual(t, len(utf16.Encode([]rune(part))), 3500, "Telegram limit exceeded")
	}
}
