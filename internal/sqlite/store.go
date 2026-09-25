package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	lock *os.File
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("database is already used by another monitor: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, lock: lock}
	if err := store.migrate(); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return errors.Join(s.db.Close(), s.lock.Close())
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;"); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 2 {
		return fmt.Errorf("unsupported database schema version %d", version)
	}
	if version == 2 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err := tx.Exec(`
CREATE TABLE state (id INTEGER PRIMARY KEY CHECK(id=1), body TEXT NOT NULL);
CREATE TABLE outbox (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL, due INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0);`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
ALTER TABLE outbox ADD COLUMN event TEXT NOT NULL DEFAULT '';
CREATE TABLE cards (episode_id TEXT PRIMARY KEY, message_id INTEGER NOT NULL);
PRAGMA user_version=2;`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Load(ctx context.Context) (*monitor.State, error) {
	state := monitor.NewState()
	var body string
	err := s.db.QueryRowContext(ctx, "SELECT body FROM state WHERE id=1").Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(body), state); err != nil {
		return nil, fmt.Errorf("invalid saved state: %w", err)
	}
	if state.Targets == nil {
		return nil, fmt.Errorf("saved state has no target map")
	}
	for _, target := range state.Targets {
		if target == nil || target.Incidents == nil {
			return nil, fmt.Errorf("saved state has an invalid target")
		}
	}
	return state, nil
}

// Save commits the incident state and pending card events atomically.
func (s *Store) Save(ctx context.Context, state *monitor.State, events []monitor.Event, now time.Time) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO state(id,body) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", string(body)); err != nil {
		return err
	}
	for _, event := range events {
		body, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO outbox(body,event,due) VALUES('',?,?)", string(body), now.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) NextNotification(ctx context.Context, now time.Time) (*monitor.Notification, error) {
	n := &monitor.Notification{}
	var body string
	// Replies must not overtake the card update they describe.
	err := s.db.QueryRowContext(ctx, "SELECT id,body,event,due,attempts FROM outbox ORDER BY id LIMIT 1").Scan(&n.ID, &n.Body, &body, &n.Due, &n.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if n.Due > now.Unix() {
		return nil, nil
	}
	if body != "" {
		n.Event = &monitor.Event{}
		if err := json.Unmarshal([]byte(body), n.Event); err != nil {
			return nil, fmt.Errorf("invalid queued event %d: %w", n.ID, err)
		}
		err := s.db.QueryRowContext(ctx, "SELECT message_id FROM cards WHERE episode_id=?", n.Event.EpisodeID).Scan(&n.MessageID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return n, nil
}

// Sent records the Telegram card ID and acknowledges delivery in one transaction.
func (s *Store) Sent(ctx context.Context, notification monitor.Notification, messageID int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if event := notification.Event; event != nil {
		switch event.Kind {
		case monitor.EventCard:
			if messageID <= 0 {
				return fmt.Errorf("invalid Telegram card message ID %d", messageID)
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO cards(episode_id,message_id) VALUES(?,?) ON CONFLICT(episode_id) DO UPDATE SET message_id=excluded.message_id", event.EpisodeID, messageID)
		case monitor.EventSummary:
			_, err = tx.ExecContext(ctx, "DELETE FROM cards WHERE episode_id=?", event.EpisodeID)
		}
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM outbox WHERE id=?", notification.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ForgetMessage(ctx context.Context, episodeID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM cards WHERE episode_id=?", episodeID)
	return err
}

func (s *Store) Retry(ctx context.Context, id int64, due time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE outbox SET due=?,attempts=attempts+1 WHERE id=?", due.Unix(), id)
	return err
}
