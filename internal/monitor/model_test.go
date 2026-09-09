package monitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	require.Empty(t, observe("1", testProbe("a", "tspu_block"), testProbe("b", "ok")), "unconfirmed alert")
	require.Equal(t, now.Add(testPolicy.ConfirmDelay), target.NextCheck, "confirmation not scheduled")
	require.Len(t, observe("2", testProbe("a", "tspu_block"), testProbe("b", "ok")), 1, "minority block not alerted")

	// Reconstruct the state exactly as after a restart.
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	state = NewState()
	require.NoError(t, json.Unmarshal(raw, state))
	target = state.Targets["node.example.com"]
	require.Empty(t, observe("3", testProbe("a", "tspu_block")), "restart duplicated alert")
	observe("4", testProbe("a", "ok"))
	observe("5", testProbe("b", "ok")) // missing affected probe breaks the healthy streak
	require.Empty(t, observe("6", testProbe("a", "ok")), "premature recovery")
	observe("7", testProbe("a", "uncertain"))
	observe("8", testProbe("a", "ok"))
	require.Len(t, observe("9", testProbe("a", "ok")), 1, "missing recovery")
	require.Empty(t, target.Incidents, "closed incident retained")
}

func TestDelayedConfirmation(t *testing.T) {
	now := time.Now()
	target := &TargetState{Incidents: make(map[string]*Incident)}
	probes := []Probe{testProbe("a", "tspu_block")}
	require.Empty(t, target.observe(Report{JobID: "1", Probes: probes}, testPolicy, now))

	// A busy queue can delay a successful follow-up beyond the regular interval.
	now = now.Add(testPolicy.Interval + time.Minute)
	require.Len(t, target.observe(Report{JobID: "2", Probes: probes}, testPolicy, now), 1)
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
	require.Empty(t, target.observe(Report{JobID: "4", Probes: []Probe{testProbe("a", "ok")}}, testPolicy, now), "error did not break recovery streak")
	moved := testProbe("a", "ok")
	moved.ASN = "AS999"
	for _, id := range []string{"5", "6"} {
		require.Empty(t, target.observe(Report{JobID: id, Probes: []Probe{moved}}, testPolicy, now), "new network healed old network")
	}
	require.Len(t, target.Incidents, 1, "old incident lost")
}

func TestRemovedTargetIsNotRecovery(t *testing.T) {
	state := NewState()
	state.sync([]Target{{Address: "node.example.com"}}, time.Now())
	state.Targets["node.example.com"].Incidents["probe"] = &Incident{Open: true}
	require.Len(t, state.sync(nil, time.Now()), 1)
	require.Empty(t, state.Targets)
}
