package monitor

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testPolicy = Policy{Interval: 30 * time.Minute, ConfirmDelay: 3 * time.Minute}

func testProbe(id string, verdicts ...string) Probe {
	return Probe{ID: id, ASN: "AS123", Region: "region", Verdicts: verdicts}
}

func testReport(id string, probes ...Probe) Report {
	return Report{JobID: id, Done: true, Online: len(probes), Probes: probes}
}

func eventKinds(events []Event) []EventKind {
	kinds := make([]EventKind, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	return kinds
}

func testTarget() *TargetState {
	return &TargetState{Target: Target{Address: "node.example.com", Names: []string{"FIN"}}, Incidents: make(map[string]*Incident)}
}

func TestConfirmationRestartAndRecovery(t *testing.T) {
	now := time.Now()
	state := NewState()
	state.sync([]Target{{Address: "node.example.com", Names: []string{"FIN"}}}, now)
	target := state.Targets["node.example.com"]
	observe := func(id string, probes ...Probe) []Event {
		now = now.Add(testPolicy.ConfirmDelay)
		return target.observe(testReport(id, probes...), testPolicy, now)
	}
	require.Empty(t, observe("1", testProbe("a", "tspu_block"), testProbe("b", "ok")))
	started := now
	require.Equal(t, now.Add(testPolicy.ConfirmDelay), target.NextCheck)
	opened := observe("2", testProbe("a", "tspu_block"), testProbe("b", "ok"))
	require.Equal(t, []EventKind{EventCard}, eventKinds(opened))
	require.False(t, opened[0].Card.Unavailable)
	require.Equal(t, started, opened[0].Card.StartedAt)
	require.NotEmpty(t, opened[0].EpisodeID)
	require.Len(t, opened[0].Card.Affected, 1)

	raw, err := json.Marshal(state)
	require.NoError(t, err)
	state = NewState()
	require.NoError(t, json.Unmarshal(raw, state))
	target = state.Targets["node.example.com"]
	updated := observe("3", testProbe("a", "tspu_block"), testProbe("b", "ok"))
	require.Equal(t, []EventKind{EventCard}, eventKinds(updated))
	require.Equal(t, opened[0].EpisodeID, updated[0].EpisodeID, "restart must update the same card")
	observe("4", testProbe("a", "ok"), testProbe("b", "ok"))
	require.Equal(t, now.Add(testPolicy.ConfirmDelay), target.NextCheck, "recovery confirmation must be prompt")
	observe("5", testProbe("b", "ok")) // the affected scanner is absent
	require.NotNil(t, target.Episode)
	observe("6", testProbe("a", "ok"), testProbe("b", "ok"))
	observe("7", testProbe("a", "uncertain"), testProbe("b", "ok"))
	observe("8", testProbe("a", "ok"), testProbe("b", "ok"))
	closed := observe("9", testProbe("a", "ok"), testProbe("b", "ok"))
	require.Equal(t, []EventKind{EventCard, EventSummary}, eventKinds(closed))
	require.Equal(t, opened[0].EpisodeID, closed[0].EpisodeID)
	require.True(t, started.Equal(closed[0].Card.StartedAt))
	require.Equal(t, now, closed[0].Card.ClosedAt)
	require.False(t, closed[0].Card.Stopped)
	require.Len(t, closed[0].Card.Affected, 1)
	require.Nil(t, target.Episode)
	require.Empty(t, target.Incidents)

	observe("10", testProbe("a", "tspu_block"), testProbe("b", "ok"))
	next := observe("11", testProbe("a", "tspu_block"), testProbe("b", "ok"))
	require.Equal(t, []EventKind{EventCard}, eventKinds(next))
	require.NotEqual(t, opened[0].EpisodeID, next[0].EpisodeID, "a new episode must not edit history")
}

func TestDelayedConfirmation(t *testing.T) {
	now := time.Now()
	target := testTarget()
	probes := []Probe{testProbe("a", "tspu_block")}
	require.Empty(t, target.observe(testReport("1", probes...), testPolicy, now))
	now = now.Add(testPolicy.Interval + time.Minute)
	require.Equal(t, []EventKind{EventCard}, eventKinds(target.observe(testReport("2", probes...), testPolicy, now)))
}

func TestErrorsAndNetworkChangesDoNotHeal(t *testing.T) {
	now := time.Now()
	target := testTarget()
	for _, id := range []string{"1", "2"} {
		target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
		now = now.Add(testPolicy.ConfirmDelay)
	}
	target.observe(testReport("3", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
	checkedAt := now
	now = now.Add(time.Minute)
	failed := target.failed(testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(failed))
	require.True(t, failed[0].Card.Unavailable)
	require.Equal(t, checkedAt, failed[0].Card.CheckedAt, "error must not refresh old results")
	require.Equal(t, now, failed[0].Card.UpdatedAt)
	require.Len(t, failed[0].Card.Probes, 2)
	require.Zero(t, target.Episode.HealthyChecks)
	require.Empty(t, target.failed(testPolicy, now.Add(time.Minute)), "repeated errors must not enqueue repeated edits")

	now = now.Add(2 * time.Minute)
	fresh := target.observe(testReport("4", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(fresh))
	require.False(t, fresh[0].Card.Unavailable)
	require.Equal(t, now, fresh[0].Card.CheckedAt)
	require.True(t, fresh[0].Card.ClosedAt.IsZero())
	moved := testProbe("a", "ok")
	moved.ASN = "AS999"
	for _, id := range []string{"5", "6"} {
		events := target.observe(testReport(id, moved, testProbe("b", "ok")), testPolicy, now)
		require.Equal(t, []EventKind{EventCard}, eventKinds(events))
		require.True(t, events[0].Card.ClosedAt.IsZero(), "new network must not heal the old network")
	}
	require.Len(t, target.Incidents, 1)
}

func TestRecoveryNeedsTwoCompleteCleanReports(t *testing.T) {
	for name, interrupted := range map[string]Report{
		"missing response": {JobID: "interrupted", Done: true, Online: 2, Probes: []Probe{testProbe("a", "ok")}},
		"unknown":          testReport("interrupted", testProbe("a", "uncertain"), testProbe("b", "ok")),
		"no online probes": testReport("interrupted"),
	} {
		t.Run(name, func(t *testing.T) {
			target := testTarget()
			now := time.Now()
			for _, id := range []string{"1", "2"} {
				target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
			}
			target.observe(testReport("3", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
			events := target.observe(interrupted, testPolicy, now)
			require.Equal(t, []EventKind{EventCard}, eventKinds(events))
			require.Zero(t, target.Episode.HealthyChecks)
			first := target.observe(testReport("4", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
			require.Equal(t, []EventKind{EventCard}, eventKinds(first))
			second := target.observe(testReport("5", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
			require.Equal(t, []EventKind{EventCard, EventSummary}, eventKinds(second))
		})
	}
}

func TestPendingBlockPreventsHostRecovery(t *testing.T) {
	target := testTarget()
	now := time.Now()
	for _, id := range []string{"1", "2"} {
		target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
	}
	id := target.Episode.ID
	target.observe(testReport("3", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
	pending := target.observe(testReport("4", testProbe("a", "ok"), testProbe("b", "tspu_block")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(pending))
	require.Equal(t, id, pending[0].EpisodeID)
	require.Len(t, target.Incidents, 1)
	require.False(t, target.Incidents[testProbe("b").key()].Open)
	target.failed(testPolicy, now)
	require.Empty(t, target.Incidents)
	require.NotNil(t, target.Episode, "the host episode must survive a failed pending check")

	unknown := target.observe(testReport("5", testProbe("a", "ok"), testProbe("b", "uncertain")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(unknown))
	first := target.observe(testReport("6", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(first))
	second := target.observe(testReport("7", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard, EventSummary}, eventKinds(second))
	require.Len(t, second[0].Card.Affected, 2)
}

func TestFullBlockEscalationIsConfirmedAndSentOnce(t *testing.T) {
	target := testTarget()
	now := time.Now()
	for _, id := range []string{"1", "2"} {
		target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
	}
	episodeID := target.Episode.ID
	first := target.observe(testReport("3", testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(first))
	require.Equal(t, now.Add(testPolicy.ConfirmDelay), target.NextCheck)

	raw, err := json.Marshal(target)
	require.NoError(t, err)
	target = testTarget()
	require.NoError(t, json.Unmarshal(raw, target))
	second := target.observe(testReport("4", testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard, EventEscalation}, eventKinds(second))
	require.Equal(t, episodeID, second[1].EpisodeID)
	require.True(t, target.Episode.FullNotified)
	for i, probes := range [][]Probe{
		{testProbe("a", "tspu_block"), testProbe("b", "tspu_block")},
		{testProbe("a", "tspu_block"), testProbe("b", "ok")},
		{testProbe("a", "tspu_block"), testProbe("b", "tspu_block")},
		{testProbe("a", "tspu_block"), testProbe("b", "tspu_block")},
	} {
		events := target.observe(testReport(fmt.Sprint(i+5), probes...), testPolicy, now)
		require.Equal(t, []EventKind{EventCard}, eventKinds(events), "one full-block notification per episode")
	}
}

func TestInitiallyFullCardDoesNotAlsoEscalate(t *testing.T) {
	target := testTarget()
	now := time.Now()
	target.observe(testReport("1", testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
	for _, id := range []string{"2", "3", "4"} {
		events := target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
		require.Equal(t, []EventKind{EventCard}, eventKinds(events))
	}
	require.True(t, target.Episode.FullNotified)
}

func TestMissingHealthyScannerDoesNotCauseFullEscalation(t *testing.T) {
	for name, incomplete := range map[string]Report{
		"healthy scanner offline": testReport("incomplete", testProbe("a", "tspu_block")),
		"missing response":        {JobID: "incomplete", Done: true, Online: 2, Probes: []Probe{testProbe("a", "tspu_block")}},
		"unknown":                 testReport("incomplete", testProbe("a", "tspu_block"), testProbe("b", "uncertain")),
		"zero online":             testReport("incomplete"),
	} {
		t.Run(name, func(t *testing.T) {
			target := testTarget()
			now := time.Now()
			for _, id := range []string{"1", "2"} {
				target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
			}
			target.observe(testReport("3", testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
			for i := range 3 {
				incomplete.JobID = fmt.Sprint(i + 4)
				events := target.observe(incomplete, testPolicy, now)
				require.Equal(t, []EventKind{EventCard}, eventKinds(events))
				require.Zero(t, target.Episode.FullChecks)
			}
			first := target.observe(testReport("7", testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
			require.Equal(t, []EventKind{EventCard}, eventKinds(first))
			second := target.observe(testReport("8", testProbe("a", "tspu_block"), testProbe("b", "tspu_block")), testPolicy, now)
			require.Equal(t, []EventKind{EventCard, EventEscalation}, eventKinds(second))
		})
	}
}

func TestLegacyEpisodeStartsFromSavedFirstDetection(t *testing.T) {
	target := testTarget()
	now := time.Now()
	firstSeen := now.Add(-2 * time.Hour)
	target.Incidents[testProbe("a").key()] = &Incident{Open: true, BadCount: 2, FirstSeen: firstSeen}
	require.Empty(t, target.failed(testPolicy, now), "legacy cards wait for a successful report")
	first := target.observe(testReport("1", testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(first))
	require.Equal(t, firstSeen, first[0].Card.StartedAt)
	require.Len(t, first[0].Card.Affected, 1)
	second := target.observe(testReport("2", testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
	require.Equal(t, first[0].EpisodeID, second[0].EpisodeID)
}

func TestEpisodeStartSurvivesRecoveryOfEarliestNetwork(t *testing.T) {
	target := testTarget()
	now := time.Now()
	started := now
	for i, probes := range [][]Probe{
		{testProbe("a", "tspu_block"), testProbe("b", "ok")},
		{testProbe("a", "tspu_block"), testProbe("b", "ok")},
		{testProbe("a", "tspu_block"), testProbe("b", "tspu_block")},
		{testProbe("a", "tspu_block"), testProbe("b", "tspu_block")},
		{testProbe("a", "ok"), testProbe("b", "tspu_block")},
		{testProbe("a", "ok"), testProbe("b", "tspu_block")},
	} {
		target.observe(testReport(fmt.Sprint(i+1), probes...), testPolicy, now)
		now = now.Add(testPolicy.ConfirmDelay)
	}
	require.NotContains(t, target.Incidents, testProbe("a").key())
	require.Equal(t, started, target.Episode.StartedAt)
	target.observe(testReport("7", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now)
	closed := target.observe(testReport("8", testProbe("a", "ok"), testProbe("b", "ok")), testPolicy, now.Add(testPolicy.ConfirmDelay))
	require.Equal(t, []EventKind{EventCard, EventSummary}, eventKinds(closed))
	require.Equal(t, started, closed[0].Card.StartedAt)
	require.Len(t, closed[0].Card.Affected, 2)
}

func TestRemovedTargetStopsWithoutClaimingRecovery(t *testing.T) {
	state := NewState()
	now := time.Now()
	state.sync([]Target{{Address: "node.example.com"}}, now)
	target := state.Targets["node.example.com"]
	for _, id := range []string{"1", "2"} {
		target.observe(testReport(id, testProbe("a", "tspu_block")), testPolicy, now)
	}
	episodeID := target.Episode.ID
	stopped := state.sync(nil, now.Add(time.Minute))
	require.Equal(t, []EventKind{EventCard, EventSummary}, eventKinds(stopped))
	require.Equal(t, episodeID, stopped[0].EpisodeID)
	require.True(t, stopped[0].Card.Stopped)
	require.False(t, stopped[0].Card.ClosedAt.IsZero())
	require.Empty(t, state.Targets)
}

func TestCardSnapshotsDoNotAliasStateOrInput(t *testing.T) {
	target := testTarget()
	now := time.Now()
	report := testReport("1", testProbe("a", "tspu_block"), testProbe("b", "ok"))
	target.observe(report, testPolicy, now)
	report.JobID = "2"
	opened := target.observe(report, testPolicy, now)
	require.Len(t, opened, 1)
	report.Probes[0].Verdicts[0] = "changed input"
	target.Target.Names[0] = "changed target"
	target.Episode.LastReport.Probes[0].Verdicts[0] = "changed state"
	probe := target.Episode.Affected[testProbe("a").key()]
	probe.Verdicts[0] = "changed history"
	require.Equal(t, "FIN", opened[0].Card.Target.Names[0])
	require.Equal(t, []string{"tspu_block"}, opened[0].Card.Probes[0].Verdicts)
	require.Equal(t, []string{"tspu_block"}, opened[0].Card.Affected[0].Verdicts)
}

func TestFailedCheckBreaksFullBlockConfirmation(t *testing.T) {
	target := testTarget()
	now := time.Now()
	for _, id := range []string{"1", "2"} {
		target.observe(testReport(id, testProbe("a", "tspu_block"), testProbe("b", "ok")), testPolicy, now)
	}
	full := []Probe{testProbe("a", "tspu_block"), testProbe("b", "tspu_block")}
	target.observe(testReport("3", full...), testPolicy, now)
	require.Equal(t, 1, target.Episode.FullChecks)
	target.failed(testPolicy, now)
	require.Zero(t, target.Episode.FullChecks)
	first := target.observe(testReport("4", full...), testPolicy, now)
	require.Equal(t, []EventKind{EventCard}, eventKinds(first))
	second := target.observe(testReport("5", full...), testPolicy, now)
	require.Equal(t, []EventKind{EventCard, EventEscalation}, eventKinds(second))
}
