package monitor

import (
	"encoding/json"
	"testing"
	"time"
)

var testPolicy = Policy{Interval: 30 * time.Minute, ConfirmDelay: 3 * time.Minute}

func testProbe(id string, verdicts ...string) Probe {
	return Probe{ID: id, ASN: "AS123", Region: "region", Verdicts: verdicts}
}

func TestConfirmationRestartAndRecovery(t *testing.T) {
	now := time.Now()
	state := NewState()
	state.sync([]Target{{Address: "node.example.com", Names: []string{"FIN"}}}, now)
	target := state.Targets["node.example.com"]
	observe := func(id string, probes ...Probe) []string {
		now = now.Add(3 * time.Minute)
		return target.observe(Report{JobID: id, Done: true, Probes: probes}, testPolicy, now)
	}
	if got := observe("1", testProbe("a", "tspu_block"), testProbe("b", "ok")); len(got) != 0 {
		t.Fatal("unconfirmed alert", got)
	}
	if !target.NextCheck.Equal(now.Add(testPolicy.ConfirmDelay)) {
		t.Fatal("confirmation not scheduled")
	}
	if got := observe("1", testProbe("a", "tspu_block")); len(got) != 0 {
		t.Fatal("same job confirmed twice")
	}
	if got := observe("2", testProbe("a", "tspu_block"), testProbe("b", "ok")); len(got) != 1 {
		t.Fatal("minority block not alerted", got)
	}
	// Reconstruct the state exactly as after a restart.
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	state = NewState()
	if err := json.Unmarshal(raw, state); err != nil {
		t.Fatal(err)
	}
	target = state.Targets["node.example.com"]
	if got := observe("3", testProbe("a", "tspu_block")); len(got) != 0 {
		t.Fatal("restart duplicated alert", got)
	}
	observe("4", testProbe("a", "ok"))
	observe("5", testProbe("b", "ok")) // missing affected probe breaks the healthy streak
	if got := observe("6", testProbe("a", "ok")); len(got) != 0 {
		t.Fatal("premature recovery", got)
	}
	observe("7", testProbe("a", "uncertain"))
	observe("8", testProbe("a", "ok"))
	if got := observe("9", testProbe("a", "ok")); len(got) != 1 {
		t.Fatal("missing recovery", got)
	}
	if len(target.Incidents) != 0 {
		t.Fatal("closed incident retained")
	}
}

func TestErrorsAndNetworkChangesDoNotHeal(t *testing.T) {
	now := time.Now()
	target := &TargetState{Incidents: make(map[string]*Incident)}
	for _, id := range []string{"1", "2"} {
		target.observe(Report{JobID: id, Probes: []Probe{testProbe("a", "tspu_block")}}, testPolicy, now)
		now = now.Add(3 * time.Minute)
	}
	target.observe(Report{JobID: "3", Probes: []Probe{testProbe("a", "ok")}}, testPolicy, now)
	target.failed(testPolicy, now)
	if got := target.observe(Report{JobID: "4", Probes: []Probe{testProbe("a", "ok")}}, testPolicy, now); len(got) != 0 {
		t.Fatal("error did not break recovery streak")
	}
	moved := testProbe("a", "ok")
	moved.ASN = "AS999"
	for _, id := range []string{"5", "6"} {
		if got := target.observe(Report{JobID: id, Probes: []Probe{moved}}, testPolicy, now); len(got) != 0 {
			t.Fatal("new network healed old network")
		}
	}
	if len(target.Incidents) != 1 {
		t.Fatal("old incident lost")
	}
}

func TestRemovedTargetIsNotRecovery(t *testing.T) {
	state := NewState()
	state.sync([]Target{{Address: "node.example.com"}}, time.Now())
	state.Targets["node.example.com"].Incidents["probe"] = &Incident{Open: true}
	messages := state.sync(nil, time.Now())
	if len(messages) != 1 || len(state.Targets) != 0 {
		t.Fatal(messages, state.Targets)
	}
}
