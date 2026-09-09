package monitor

import (
	"fmt"
	"slices"
	"sort"
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

type TargetState struct {
	Target    Target
	NextCheck time.Time
	LastJob   string
	Failures  int
	Incidents map[string]*Incident
}

type State struct {
	Targets   map[string]*TargetState
	NextProbe time.Time
}

func NewState() *State {
	return &State{Targets: make(map[string]*TargetState)}
}

func (s *State) sync(targets []Target, now time.Time) []string {
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
	var messages []string
	for address, target := range s.Targets {
		if present[address] {
			continue
		}
		for _, incident := range target.Incidents {
			if incident.Open {
				messages = append(messages, "Наблюдение прекращено: "+address+". Цель удалена, отключена или больше не подходит для проверки. Восстановление не подтверждено.")
				break
			}
		}
		delete(s.Targets, address)
	}
	return messages
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

func (t *TargetState) failed(policy Policy, now time.Time) {
	t.Failures++
	delay := time.Minute * time.Duration(1<<min(t.Failures-1, 4))
	t.NextCheck = now.Add(min(policy.Interval, delay))
	for key, incident := range t.Incidents {
		incident.GoodCount = 0
		if !incident.Open {
			delete(t.Incidents, key)
		}
	}
}

// observe applies one completed scan. A missing or uncertain probe never heals an incident.
func (t *TargetState) observe(report Report, policy Policy, now time.Time) []string {
	t.NextCheck = now.Add(policy.Interval)
	t.LastJob = report.JobID
	t.Failures = 0
	seen := make(map[string]bool, len(report.Probes))
	var changes []string
	for _, probe := range report.Probes {
		key := probe.key()
		seen[key] = true
		incident := t.Incidents[key]
		if slices.Contains(probe.Verdicts, "tspu_block") {
			if incident == nil {
				incident = &Incident{Network: probe.label(), FirstSeen: now}
				t.Incidents[key] = incident
			}
			incident.GoodCount = 0
			incident.BadCount++
			if !incident.Open {
				if incident.BadCount >= 2 {
					incident.Open = true
					changes = append(changes, fmt.Sprintf("ТСПУ: блокировка подтверждена\nСеть: %s\nПервое обнаружение: %s", incident.Network, incident.FirstSeen.UTC().Format(time.RFC3339)))
				} else {
					t.NextCheck = now.Add(policy.ConfirmDelay)
				}
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
		if len(probe.Verdicts) != 1 || probe.Verdicts[0] != "ok" {
			incident.GoodCount = 0
			continue
		}
		incident.GoodCount++
		if incident.GoodCount >= 2 {
			changes = append(changes, fmt.Sprintf("ТСПУ: доступ восстановлен\nСеть: %s\nДлительность: %s", incident.Network, now.Sub(incident.FirstSeen).Round(time.Minute)))
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
	sort.Strings(changes)
	return changes
}
