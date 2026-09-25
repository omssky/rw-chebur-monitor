package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

// Interfaces belong to the consumer; adapters do not depend on each other.
type Discoverer interface {
	Discover(context.Context) ([]Target, []string, error)
}

type Checker interface {
	Check(context.Context, string) (Report, error)
}

type Sender interface {
	Send(context.Context, Notification) (int, error)
}

type Repository interface {
	Load(context.Context) (*State, error)
	Save(context.Context, *State, []Event, time.Time) error
	NextNotification(context.Context, time.Time) (*Notification, error)
	Sent(context.Context, Notification, int) error
	ForgetMessage(context.Context, string) error
	Retry(context.Context, int64, time.Time) error
}

type Notification struct {
	ID        int64
	Body      string
	Due       int64
	Attempts  int
	Event     *Event
	MessageID int
}

type MessageMissingError struct{ Err error }

func (e *MessageMissingError) Error() string { return e.Err.Error() }
func (e *MessageMissingError) Unwrap() error { return e.Err }

type RateLimitError struct{ After time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited for %s", e.After)
}

type Service struct {
	Discovery Discoverer
	Checker   Checker
	Sender    Sender
	Store     Repository
	Policy    Policy
	Log       *slog.Logger
}

func (s *Service) Run(ctx context.Context) error {
	state, err := s.Store.Load(ctx)
	if err != nil {
		return err
	}
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return s.poll(ctx, state) })
	group.Go(func() error { return s.deliver(ctx) })
	err = group.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (s *Service) poll(ctx context.Context, state *State) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var nextDiscovery time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		now := time.Now()
		if !now.Before(nextDiscovery) {
			targets, warnings, err := s.Discovery.Discover(ctx)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			nextDiscovery = now.Add(s.Policy.Interval)
			if err != nil {
				s.Log.Warn("Discovery failed; keeping saved targets", "error", err)
				nextDiscovery = now.Add(min(5*time.Minute, s.Policy.Interval))
			} else {
				for _, warning := range warnings {
					s.Log.Warn("Target discovery", "detail", warning)
				}
				messages := state.sync(targets, now)
				if err := s.Store.Save(ctx, state, messages, now); err != nil {
					return err
				}
				s.Log.Info("Targets synchronized", "count", len(targets))
			}
		}
		now = time.Now()
		if now.Before(state.NextProbe) {
			continue
		}
		target := state.due(now)
		if target == nil {
			continue
		}
		// Persist pacing before the request, so a restart cannot immediately flood the API.
		state.NextProbe = now.Add(15 * time.Second)
		if err := s.Store.Save(ctx, state, nil, now); err != nil {
			return err
		}
		report, err := s.Checker.Check(ctx, target.Target.Address)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now = time.Now().UTC()
		var messages []Event
		if err == nil && (!report.Done || report.JobID == "") {
			err = errors.New("incomplete check")
		}
		if err == nil && report.JobID == target.LastJob {
			err = errors.New("repeated check job")
		}
		if err != nil {
			messages = target.failed(s.Policy, now)
			var limit *RateLimitError
			if errors.As(err, &limit) {
				state.NextProbe = now.Add(max(time.Minute, limit.After))
				target.NextCheck = state.NextProbe
			}
			s.Log.Warn("Check failed", "target", target.Target.Address, "error", err)
		} else {
			messages = target.observe(report, s.Policy, now)
			s.Log.Info("Check completed", "target", target.Target.Address, "probes", len(report.Probes), "online", report.Online, "events", len(messages))
		}
		if err := s.Store.Save(ctx, state, messages, now); err != nil {
			return err
		}
	}
}

func (s *Service) deliver(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			notification, err := s.Store.NextNotification(ctx, now)
			if err != nil {
				return err
			}
			if notification == nil {
				continue
			}
			messageID, err := s.Sender.Send(ctx, *notification)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				if err := s.Store.Sent(ctx, *notification, messageID); err != nil {
					return err
				}
				s.Log.Info("Notification delivered", "id", notification.ID)
				continue
			}
			var missing *MessageMissingError
			if errors.As(err, &missing) && notification.Event != nil && notification.Event.Kind == EventCard {
				if err := s.Store.ForgetMessage(ctx, notification.Event.EpisodeID); err != nil {
					return err
				}
				if err := s.Store.Retry(ctx, notification.ID, time.Now()); err != nil {
					return err
				}
				s.Log.Warn("Incident card missing; recreating", "id", notification.ID)
				continue
			}
			delay := min(time.Hour, 5*time.Second*time.Duration(1<<min(notification.Attempts, 10)))
			var limit *RateLimitError
			if errors.As(err, &limit) {
				delay = max(delay, limit.After)
			}
			s.Log.Warn("Notification delayed", "id", notification.ID, "retry_in", delay, "error", err)
			if err := s.Store.Retry(ctx, notification.ID, time.Now().Add(delay)); err != nil {
				return err
			}
		}
	}
}
