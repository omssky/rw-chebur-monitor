package telegram

import (
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
	"github.com/stretchr/testify/require"
)

func sampleCard() monitor.Card {
	start := time.Date(2026, time.September, 25, 10, 0, 0, 0, time.UTC)
	return monitor.Card{
		Target:    monitor.Target{Address: "pl-node.example.com", Names: []string{"Poland"}},
		StartedAt: start, CheckedAt: start.Add(2 * time.Hour), UpdatedAt: start.Add(2 * time.Hour), Online: 5,
		Probes: []monitor.Probe{
			{ID: "healthy", Provider: "A healthy", Region: "Москва", Verdicts: []string{"ok"}},
			{ID: "unknown", Provider: "B unknown", Region: "Москва", Verdicts: []string{"uncertain"}},
			{ID: "bad1", Provider: "C blocked", ASN: "AS1", Region: "Москва", Verdicts: []string{"tspu_block"}},
			{ID: "bad2", Provider: "C blocked", ASN: "AS1", Region: "Москва", Verdicts: []string{"tspu_block"}},
		},
		Affected: []monitor.Probe{{ID: "gone", Provider: "D missing", Region: "Омск", Verdicts: []string{"tspu_block"}}},
	}
}

func TestCardCoverageGroupingAndMissingProbes(t *testing.T) {
	card := sampleCard()
	text := render(monitor.Event{Kind: monitor.EventCard, Card: card})
	require.Contains(t, text, "<p><b>Poland</b> · <code>pl-node.example.com</code></p>")
	require.Contains(t, text, "🔎 Блокировка у <b>2 из 4</b> ответивших сканеров")
	require.Contains(t, text, "Нет ответа: <b>1</b>")
	require.Contains(t, text, "<td>блокировка: 2</td>")
	require.Contains(t, text, "<td>доступен: 1</td>")
	require.Contains(t, text, "<td>неопределённо: 1</td>")
	require.Contains(t, text, "<td>нет ответа: 1</td>")
	require.Contains(t, text, "🕒 Наблюдаем <b>2 ч</b> · с 25.09, 13:00 МСК")
	require.Contains(t, text, "<summary>Сети и регионы</summary>")
	require.NotContains(t, text, "<h3>")
	require.Equal(t, 1, strings.Count(text, "C blocked AS1"))
	require.Less(t, strings.Index(text, "C blocked AS1"), strings.Index(text, "B unknown"))
	require.Less(t, strings.Index(text, "B unknown"), strings.Index(text, "A healthy"))
	require.Contains(t, text, "D missing")
	require.Contains(t, text, "25.09, 13:00 МСК")
	require.Equal(t, []string{"tspu_block"}, card.Affected[0].Verdicts, "formatting must not modify the snapshot")
}

func TestClosedCardKeepsHistoryAndStrikesOnlyInlineContent(t *testing.T) {
	card := sampleCard()
	card.ClosedAt = card.UpdatedAt
	card.Affected[0].Verdicts = []string{"ok"}
	text := render(monitor.Event{Kind: monitor.EventCard, Card: card})
	require.Contains(t, text, "<p><b>Poland · восстановлено</b></p>")
	require.Contains(t, text, "<summary><s>Сети, затронутые за время инцидента</s></summary>")
	require.Contains(t, text, "<td><s>D missing</s></td>")
	require.Contains(t, text, "Блокировалось: 1")
	decoder := xml.NewDecoder(strings.NewReader("<root>" + text + "</root>"))
	var stack []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Local == "s" {
				require.Contains(t, []string{"p", "summary", "th", "td"}, stack[len(stack)-1])
			}
			stack = append(stack, token.Name.Local)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
}

func TestUnavailableAndStoppedNeverClaimRecovery(t *testing.T) {
	card := sampleCard()
	card.Unavailable = true
	card.UpdatedAt = card.CheckedAt.Add(time.Hour)
	text := render(monitor.Event{Kind: monitor.EventCard, Card: card})
	require.Contains(t, text, "Последняя успешная проверка: 25.09, 15:00 МСК")
	require.NotContains(t, text, "Проверено: 25.09, 16:00")
	require.Contains(t, text, "Нет свежих данных")
	card.Stopped = true
	card.ClosedAt = card.UpdatedAt
	for _, kind := range []monitor.EventKind{monitor.EventCard, monitor.EventSummary} {
		text = render(monitor.Event{Kind: kind, Card: card})
		require.Contains(t, text, "наблюдение прекращено")
		require.Contains(t, text, "Восстановление не подтверждено")
		require.NotContains(t, text, "восстановлено")
		require.NotContains(t, text, "доступ восстановлен")
		require.NotContains(t, text, "✓")
	}
}

func TestSummaryAndEscalation(t *testing.T) {
	card := sampleCard()
	card.ClosedAt = card.UpdatedAt
	summary := render(monitor.Event{Kind: monitor.EventSummary, Card: card})
	require.Contains(t, summary, "<b>✓ Poland — доступ восстановлен</b>")
	require.Contains(t, summary, "Период наблюдения: <b>2 ч</b>.")
	require.NotContains(t, summary, "<s>")
	card.Probes = card.Probes[2:]
	escalation := render(monitor.Event{Kind: monitor.EventEscalation, Card: card})
	require.Equal(t, "<p><b>Poland</b> — теперь блокировка у всех ответивших сканеров (<b>2</b>).</p>", escalation)
}

func TestNestedHTMLValuesAndLinkAreEscaped(t *testing.T) {
	card := sampleCard()
	card.Target.Names = []string{`<s>name & "quote"</s>`}
	card.Target.Address = `host.example/?x=" onmouseover="bad"&y=<script>`
	card.Affected[0].Provider = `<b>provider</b>`
	card.Affected[0].Region = `region & "x"`
	card.ClosedAt = card.UpdatedAt
	text := render(monitor.Event{Kind: monitor.EventCard, Card: card})
	require.Contains(t, text, html.EscapeString(card.Target.Names[0]))
	require.Contains(t, text, "<td><s>"+html.EscapeString(card.Affected[0].Provider)+"</s></td>")
	require.Contains(t, text, html.EscapeString(card.Affected[0].Region))
	require.NotContains(t, text, `<script>`)
	require.NotContains(t, text, `onmouseover="bad"`)
	require.Contains(t, text, `href="https://cheburcheck.ru/check?target=host.example%2F%3Fx%3D%22`)
}

func TestLargeProbeInventoryStaysWithinRichMessageLimits(t *testing.T) {
	card := sampleCard()
	card.Probes = nil
	card.Affected = nil
	for i := range 600 {
		card.Probes = append(card.Probes, monitor.Probe{ID: fmt.Sprint(i), Provider: fmt.Sprint(i) + strings.Repeat("я", 300), Region: strings.Repeat("я", 300), Verdicts: []string{"tspu_block"}})
	}
	text := render(monitor.Event{Kind: monitor.EventCard, Card: card})
	require.Contains(t, text, "Показаны первые 80 из 600")
	require.Less(t, utf8.RuneCountInString(text), 32768)
	require.Equal(t, 81, strings.Count(text, "<tr>"))
}

func TestPendingRecoveryAndUnknownDates(t *testing.T) {
	card := sampleCard()
	card.Probes = card.Probes[:1]
	card.CheckedAt = time.Time{}
	text := render(monitor.Event{Kind: monitor.EventCard, Card: card})
	require.Contains(t, text, "🔎 Восстановление не подтверждено")
	require.Contains(t, text, "Проверено: нет данных")
	require.NotContains(t, text, "01.01.0001")
}
