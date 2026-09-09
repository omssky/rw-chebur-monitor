package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
)

func TestRestartAndOrderedDelivery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "monitor.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	state := monitor.NewState()
	state.Targets["node.example.com"] = &monitor.TargetState{
		Target:    monitor.Target{Address: "node.example.com"},
		Incidents: map[string]*monitor.Incident{"probe": {Open: true}},
	}
	if err := store.Save(ctx, state, []string{"opening"}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.Load(ctx)
	if err != nil || !loaded.Targets["node.example.com"].Incidents["probe"].Open {
		t.Fatal("incident lost", err)
	}
	first, err := store.NextNotification(ctx, now)
	if err != nil || first == nil {
		t.Fatal("notification lost", err)
	}
	if err := store.Retry(ctx, first.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, loaded, []string{"recovery"}, now); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextNotification(ctx, now); err != nil || next != nil {
		t.Fatal("recovery overtook opening", next, err)
	}
	if err := store.Sent(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := store.NextNotification(ctx, now); err != nil || next == nil || next.Body != "recovery" {
		t.Fatal(next, err)
	}
}

func TestTransactionRollbackAndExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if second, err := Open(path); err == nil {
		second.Close()
		t.Fatal("second instance acquired database")
	}
	ctx := context.Background()
	state := monitor.NewState()
	if err := store.Save(ctx, state, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END;`); err != nil {
		t.Fatal(err)
	}
	state.NextProbe = time.Now()
	if err := store.Save(ctx, state, []string{"alert"}, time.Now()); err == nil {
		t.Fatal("expected transaction failure")
	}
	loaded, err := store.Load(ctx)
	if err != nil || !loaded.NextProbe.IsZero() {
		t.Fatal("state committed without alert", err)
	}
}

func TestTelegramMessageLimit(t *testing.T) {
	message := strings.Repeat("🇫🇮 node ", 600)
	parts := splitMessage(message)
	if strings.Join(parts, "") != message {
		t.Fatal("message corrupted")
	}
	for _, part := range parts {
		if len(utf16.Encode([]rune(part))) > 3500 {
			t.Fatal("Telegram limit exceeded")
		}
	}
}
