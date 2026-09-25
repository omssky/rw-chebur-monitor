package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyOutbox(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "monitor.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE state (id INTEGER PRIMARY KEY CHECK(id=1), body TEXT NOT NULL);
CREATE TABLE outbox (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL, due INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0);
INSERT INTO state(id,body) VALUES(1,'{"Targets":{}}');
PRAGMA user_version=1;`)
	require.NoError(t, err)
	now := time.Now()
	due := now.Add(time.Minute)
	_, err = db.Exec("INSERT INTO outbox(body,due,attempts) VALUES(?,?,?)", "legacy alert", due.Unix(), 3)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store := openStore(t, path)
	var version int
	require.NoError(t, store.db.QueryRow("PRAGMA user_version").Scan(&version))
	require.Equal(t, 2, version)
	state, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, state.Targets)
	next, err := store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.Nil(t, next, "migration changed the retry delay")

	next, err = store.NextNotification(ctx, due)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "legacy alert", next.Body)
	require.Equal(t, 3, next.Attempts)
	require.Nil(t, next.Event)
	require.NoError(t, store.Sent(ctx, *next, 777))
	next, err = store.NextNotification(ctx, due)
	require.NoError(t, err)
	require.Nil(t, next)
}

func TestCardReferenceSurvivesRestartAndStateSave(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "monitor.db")
	store := openStore(t, path)
	now := time.Now().UTC()
	state := monitor.NewState()
	event := monitor.Event{
		Kind:      monitor.EventCard,
		EpisodeID: "episode-1",
		Card: monitor.Card{
			Target:    monitor.Target{Address: "node.example.com"},
			StartedAt: now,
		},
	}
	require.NoError(t, store.Save(ctx, state, []monitor.Event{event}, now))
	first, err := store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, &event, first.Event)
	require.Zero(t, first.MessageID)
	require.NoError(t, store.Sent(ctx, *first, 101))
	require.NoError(t, store.Close())

	store = openStore(t, path)
	// Polling saves its own snapshot and cannot overwrite delivery's card ID.
	state.NextProbe = now.Add(time.Minute)
	event.Card.CheckedAt = now.Add(time.Minute)
	require.NoError(t, store.Save(ctx, state, []monitor.Event{event}, now))
	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, state.NextProbe, loaded.NextProbe)
	next, err := store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, 101, next.MessageID)

	require.NoError(t, store.ForgetMessage(ctx, event.EpisodeID))
	next, err = store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Zero(t, next.MessageID, "deleted card must be recreated")
	require.NoError(t, store.Sent(ctx, *next, 202))
	require.NoError(t, store.Save(ctx, loaded, []monitor.Event{event}, now))
	next, err = store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, 202, next.MessageID)
}

func TestOrderedCardAndReplies(t *testing.T) {
	ctx := t.Context()
	store := openStore(t, filepath.Join(t.TempDir(), "monitor.db"))
	now := time.Now().UTC()
	events := []monitor.Event{
		{Kind: monitor.EventCard, EpisodeID: "episode-1"},
		{Kind: monitor.EventEscalation, EpisodeID: "episode-1"},
		{Kind: monitor.EventCard, EpisodeID: "episode-1", Card: monitor.Card{ClosedAt: now}},
		{Kind: monitor.EventSummary, EpisodeID: "episode-1"},
	}
	require.NoError(t, store.Save(ctx, monitor.NewState(), events, now))
	for i, event := range events {
		next, err := store.NextNotification(ctx, now)
		require.NoError(t, err)
		require.NotNil(t, next)
		require.Equal(t, &event, next.Event)
		if i == 0 {
			require.Zero(t, next.MessageID)
			require.NoError(t, store.Retry(ctx, next.ID, now.Add(time.Minute)))
			blocked, err := store.NextNotification(ctx, now)
			require.NoError(t, err)
			require.Nil(t, blocked, "reply overtook the delayed card")
		} else {
			require.Equal(t, 101, next.MessageID)
		}
		messageID := 101
		if event.Kind != monitor.EventCard {
			messageID = 999 // Replies must never replace the card ID.
		}
		require.NoError(t, store.Sent(ctx, *next, messageID))
	}
	var count int
	require.NoError(t, store.db.QueryRow("SELECT COUNT(*) FROM cards").Scan(&count))
	require.Zero(t, count, "completed episode retained its card reference")
	next, err := store.NextNotification(ctx, now)
	require.NoError(t, err)
	require.Nil(t, next)
}

func TestSaveRollbackAndExclusiveLock(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "monitor.db")
	store := openStore(t, path)
	second, err := Open(path)
	if second != nil {
		defer second.Close()
	}
	require.Error(t, err, "second instance acquired database")

	state := monitor.NewState()
	require.NoError(t, store.Save(ctx, state, nil, time.Now()))
	_, err = store.db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END;`)
	require.NoError(t, err)
	state.NextProbe = time.Now()
	event := monitor.Event{Kind: monitor.EventCard, EpisodeID: "episode-1"}
	require.Error(t, store.Save(ctx, state, []monitor.Event{event}, time.Now()))
	loaded, err := store.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.True(t, loaded.NextProbe.IsZero(), "state committed without card event")
}

func TestAcknowledgementIsAtomic(t *testing.T) {
	ctx := t.Context()
	store := openStore(t, filepath.Join(t.TempDir(), "monitor.db"))
	now := time.Now()
	state := monitor.NewState()
	for _, kind := range []monitor.EventKind{monitor.EventCard, monitor.EventSummary} {
		event := monitor.Event{Kind: kind, EpisodeID: "episode-1"}
		require.NoError(t, store.Save(ctx, state, []monitor.Event{event}, now))
		next, err := store.NextNotification(ctx, now)
		require.NoError(t, err)
		require.NotNil(t, next)
		_, err = store.db.Exec(`CREATE TRIGGER reject_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END;`)
		require.NoError(t, err)
		require.Error(t, store.Sent(ctx, *next, 101))

		pending, err := store.NextNotification(ctx, now)
		require.NoError(t, err)
		require.NotNil(t, pending)
		require.Equal(t, next.ID, pending.ID)
		require.Equal(t, next.MessageID, pending.MessageID, "reference changed without acknowledging delivery")
		_, err = store.db.Exec("DROP TRIGGER reject_ack")
		require.NoError(t, err)
		require.NoError(t, store.Sent(ctx, *next, 101))
	}
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}
