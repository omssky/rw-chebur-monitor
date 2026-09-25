package monitor

import "time"

// Event is a durable change to an incident's Telegram card or its closing reply.
type Event struct {
	Kind      EventKind
	EpisodeID string
	Card      Card
}

type EventKind string

const (
	EventCard       EventKind = "card"
	EventSummary    EventKind = "summary"
	EventEscalation EventKind = "escalation"
)

// Card contains a snapshot; queued updates never read mutable monitoring state.
type Card struct {
	Target      Target
	StartedAt   time.Time
	CheckedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    time.Time
	Unavailable bool
	Stopped     bool
	Online      int
	Probes      []Probe
	Affected    []Probe
}
