package monitor

import (
	"crypto/rand"
	"slices"
	"strings"
	"time"
)

type Target struct {
	Address string
	Names   []string
}

type Probe struct {
	ID       string   `json:"probe_id"`
	JobID    string   `json:"job_id"`
	Provider string   `json:"provider"`
	Region   string   `json:"region"`
	ASN      string   `json:"asn"`
	Verdicts []string `json:"verdicts"`
}

func (p Probe) key() string {
	return p.ID + "|" + p.ASN + "|" + p.Region
}

func (p Probe) label() string {
	parts := []string{p.Provider, p.ASN, p.Region}
	parts = slices.DeleteFunc(parts, func(s string) bool { return s == "" })
	return strings.Join(parts, " / ") + " (сканер " + p.ID + ")"
}

type Report struct {
	JobID  string
	Target string
	Done   bool
	Online int
	Probes []Probe
}

type Policy struct {
	Interval     time.Duration
	ConfirmDelay time.Duration
}

type Incident struct {
	Network   string
	FirstSeen time.Time
	Open      bool
	BadCount  int
	GoodCount int
}

type Episode struct {
	ID            string
	StartedAt     time.Time
	CheckedAt     time.Time
	LastReport    Report
	Affected      map[string]Probe
	Known         map[string]bool
	HealthyChecks int
	FullChecks    int
	// One full-block notification per episode; an initially full card counts.
	FullNotified bool
}

type TargetState struct {
	Target    Target
	NextCheck time.Time
	LastJob   string
	Failures  int
	Incidents map[string]*Incident
	Episode   *Episode
}

type State struct {
	Targets   map[string]*TargetState
	NextProbe time.Time
}

func NewState() *State {
	return &State{Targets: make(map[string]*TargetState)}
}

func (s *State) sync(targets []Target, now time.Time) []Event {
	present := make(map[string]bool, len(targets))
	for _, target := range targets {
		present[target.Address] = true
		if old := s.Targets[target.Address]; old != nil {
			old.Target = target
			continue
		}
		s.Targets[target.Address] = &TargetState{
			Target: target, NextCheck: now, Incidents: make(map[string]*Incident),
		}
	}
	var events []Event
	for address, target := range s.Targets {
		if present[address] {
			continue
		}
		target.startEpisode(now)
		if target.Episode != nil {
			events = append(events, target.closeEpisode(now, true)...)
		}
		delete(s.Targets, address)
	}
	return events
}

func (s *State) due(now time.Time) *TargetState {
	var next *TargetState
	for _, target := range s.Targets {
		if target.NextCheck.After(now) {
			continue
		}
		if next == nil || target.NextCheck.Before(next.NextCheck) ||
			(target.NextCheck.Equal(next.NextCheck) && target.Target.Address < next.Target.Address) {
			next = target
		}
	}
	return next
}

func (t *TargetState) failed(policy Policy, now time.Time) []Event {
	alreadyUnavailable := t.Failures > 0
	t.Failures++
	delay := time.Minute * time.Duration(1<<min(t.Failures-1, 4))
	t.NextCheck = now.Add(min(policy.Interval, delay))
	for key, incident := range t.Incidents {
		incident.GoodCount = 0
		if !incident.Open {
			delete(t.Incidents, key)
		}
	}
	if t.Episode == nil {
		return nil
	}
	t.Episode.HealthyChecks = 0
	t.Episode.FullChecks = 0
	if alreadyUnavailable {
		return nil
	}
	return []Event{{Kind: EventCard, EpisodeID: t.Episode.ID, Card: t.card(now)}}
}

// observe applies one completed scan. Missing or uncertain probes never heal an incident.
func (t *TargetState) observe(report Report, policy Policy, now time.Time) []Event {
	t.NextCheck = now.Add(policy.Interval)
	t.LastJob = report.JobID
	t.Failures = 0
	// Older snapshots contain open probe incidents but no host episode yet.
	created := t.startEpisode(now)
	seen := make(map[string]bool, len(report.Probes))
	allBlocked := report.Online > 0 && len(report.Probes) == report.Online
	allHealthy := allBlocked
	for _, probe := range report.Probes {
		key := probe.key()
		seen[key] = true
		blocked := slices.Contains(probe.Verdicts, "tspu_block")
		healthy := len(probe.Verdicts) == 1 && probe.Verdicts[0] == "ok"
		allBlocked = allBlocked && blocked
		allHealthy = allHealthy && healthy
		incident := t.Incidents[key]
		if blocked {
			if incident == nil {
				incident = &Incident{Network: probe.label(), FirstSeen: now}
				t.Incidents[key] = incident
			}
			incident.GoodCount = 0
			incident.BadCount = min(2, incident.BadCount+1)
			if incident.BadCount >= 2 {
				incident.Open = true
			} else if !incident.Open {
				t.NextCheck = now.Add(policy.ConfirmDelay)
			}
			continue
		}
		if incident == nil {
			continue
		}
		if !incident.Open {
			delete(t.Incidents, key)
			continue
		}
		if !healthy {
			incident.GoodCount = 0
			continue
		}
		incident.GoodCount++
		if incident.GoodCount >= 2 {
			delete(t.Incidents, key)
		}
	}
	for key, incident := range t.Incidents {
		if !seen[key] {
			incident.GoodCount = 0
			if !incident.Open {
				delete(t.Incidents, key)
			}
		}
	}
	created = t.startEpisode(now) || created
	if t.Episode == nil {
		return nil
	}
	episode := t.Episode
	report.Probes = copyProbes(report.Probes)
	episode.LastReport = report
	episode.CheckedAt = now
	for _, probe := range report.Probes {
		key := probe.key()
		episode.Known[key] = true
		if _, affected := episode.Affected[key]; affected || slices.Contains(probe.Verdicts, "tspu_block") {
			probe.Verdicts = slices.Clone(probe.Verdicts)
			episode.Affected[key] = probe
		}
	}
	// A healthy scanner going offline must not turn a partial block into a full one.
	for key := range episode.Known {
		if !seen[key] {
			allBlocked = false
		}
	}
	if allHealthy {
		episode.HealthyChecks = min(2, episode.HealthyChecks+1)
	} else {
		episode.HealthyChecks = 0
	}
	if allBlocked {
		episode.FullChecks = min(2, episode.FullChecks+1)
	} else {
		episode.FullChecks = 0
	}
	if created && allBlocked {
		episode.FullNotified = true
	}
	if episode.HealthyChecks >= 2 && len(t.Incidents) == 0 {
		return t.closeEpisode(now, false)
	}
	if episode.HealthyChecks == 1 || episode.FullChecks == 1 && !episode.FullNotified {
		t.NextCheck = now.Add(policy.ConfirmDelay)
	}
	card := t.card(now)
	events := []Event{{Kind: EventCard, EpisodeID: episode.ID, Card: card}}
	if episode.FullChecks >= 2 && !episode.FullNotified {
		episode.FullNotified = true
		events = append(events, Event{Kind: EventEscalation, EpisodeID: episode.ID, Card: card})
	}
	return events
}

// startEpisode also upgrades saved probe incidents without emitting duplicate openings.
func (t *TargetState) startEpisode(now time.Time) bool {
	if t.Episode != nil {
		return false
	}
	var episode *Episode
	for key, incident := range t.Incidents {
		if !incident.Open {
			continue
		}
		if episode == nil {
			episode = &Episode{ID: rand.Text(), Affected: make(map[string]Probe), Known: make(map[string]bool)}
		}
		if !incident.FirstSeen.IsZero() && (episode.StartedAt.IsZero() || incident.FirstSeen.Before(episode.StartedAt)) {
			episode.StartedAt = incident.FirstSeen
		}
		// The old format kept scanner identity in the map key; newer reports fill its metadata.
		parts := strings.SplitN(key, "|", 3)
		probe := Probe{ID: parts[0]}
		if len(parts) == 3 {
			probe.ASN, probe.Region = parts[1], parts[2]
		}
		episode.Affected[key] = probe
		episode.Known[key] = true
	}
	if episode == nil {
		return false
	}
	if episode.StartedAt.IsZero() {
		episode.StartedAt = now
	}
	t.Episode = episode
	return true
}

func (t *TargetState) closeEpisode(now time.Time, stopped bool) []Event {
	card := t.card(now)
	card.ClosedAt, card.Stopped = now, stopped
	events := []Event{
		{Kind: EventCard, EpisodeID: t.Episode.ID, Card: card},
		{Kind: EventSummary, EpisodeID: t.Episode.ID, Card: card},
	}
	t.Episode = nil
	return events
}

func (t *TargetState) card(now time.Time) Card {
	episode := t.Episode
	affected := make([]Probe, 0, len(episode.Affected))
	for _, probe := range episode.Affected {
		affected = append(affected, probe)
	}
	target := t.Target
	target.Names = slices.Clone(target.Names)
	return Card{
		Target: target, StartedAt: episode.StartedAt, CheckedAt: episode.CheckedAt, UpdatedAt: now,
		Unavailable: t.Failures > 0 || episode.CheckedAt.IsZero(),
		Online:      episode.LastReport.Online, Probes: copyProbes(episode.LastReport.Probes), Affected: copyProbes(affected),
	}
}

func copyProbes(probes []Probe) []Probe {
	copied := slices.Clone(probes)
	for i := range copied {
		copied[i].Verdicts = slices.Clone(copied[i].Verdicts)
	}
	slices.SortFunc(copied, func(a, b Probe) int { return strings.Compare(a.key(), b.key()) })
	return copied
}
