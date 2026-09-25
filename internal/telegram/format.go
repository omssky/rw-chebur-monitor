package telegram

import (
	"fmt"
	"html"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/omssky/rw-chebur-monitor/internal/monitor"
)

var moscow = time.FixedZone("МСК", 3*60*60)

func render(event monitor.Event) string {
	card := event.Card
	host := strings.Join(card.Target.Names, ", ")
	if host == "" {
		host = card.Target.Address
	}
	host = escape(host, 160)
	address := "<code>" + escape(card.Target.Address, 253) + "</code>"
	link := `https://cheburcheck.ru/check?target=` + url.QueryEscape(card.Target.Address)
	link = html.EscapeString(link)
	end := card.UpdatedAt
	if !card.ClosedAt.IsZero() {
		end = card.ClosedAt
	}

	if event.Kind == monitor.EventEscalation {
		return fmt.Sprintf("<p><b>%s</b> — теперь блокировка у всех ответивших сканеров (<b>%d</b>).</p>", host, len(card.Probes))
	}
	if event.Kind == monitor.EventSummary {
		heading := "✓ " + host + " — доступ восстановлен"
		text := ""
		if card.Stopped {
			heading = host + " — наблюдение прекращено"
			text = "<p>Восстановление не подтверждено.</p>"
		}
		return "<p><b>" + heading + "</b></p>" + text +
			"<p>Период наблюдения: <b>" + duration(end.Sub(card.StartedAt)) + "</b>.</p>"
	}

	blocked := 0
	for _, probe := range card.Probes {
		if probeStatus(probe) == 0 {
			blocked++
		}
	}

	closed := !card.ClosedAt.IsZero()
	var b strings.Builder
	heading := "<b>" + host + "</b> · " + address
	switch {
	case card.Stopped:
		heading = "<b>" + host + " · наблюдение прекращено</b>"
	case closed:
		heading = "<b>" + host + " · восстановлено</b>"
	}
	fmt.Fprintf(&b, "<p>%s</p>", heading)
	// Strikethrough stays inside each block: wrapping a table or details in <s> is invalid.
	inline := func(text string) string {
		if closed {
			return "<s>" + text + "</s>"
		}
		return text
	}
	paragraph := func(text string) { fmt.Fprintf(&b, "<p>%s</p>", inline(text)) }
	if closed {
		paragraph(address)
	}
	switch {
	case card.Stopped:
		paragraph("Восстановление не подтверждено.")
	case closed:
		// The closed card keeps the incident's history in the details below.
	case card.Unavailable:
		paragraph("🔎 Нет свежих данных · ниже последние результаты")
	default:
		status := "🔎 Восстановление не подтверждено"
		if blocked > 0 {
			label := "сканеров"
			if card.Online > len(card.Probes) {
				label = "ответивших сканеров"
			}
			status = fmt.Sprintf("🔎 Блокировка у <b>%d из %d</b> %s", blocked, len(card.Probes), label)
		}
		paragraph(status)
		if missing := card.Online - len(card.Probes); missing > 0 {
			paragraph(fmt.Sprintf("Нет ответа: <b>%d</b>", missing))
		}
	}
	period := "🕒 Наблюдаем "
	if closed {
		period = "Период наблюдения: "
	}
	paragraph(period + "<b>" + duration(end.Sub(card.StartedAt)) + "</b> · с " + timestamp(card.StartedAt))
	checked := "Проверено: "
	if card.Unavailable || card.Stopped {
		checked = "Последняя успешная проверка: "
	}
	paragraph(checked + timestamp(card.CheckedAt))
	if closed {
		paragraph("Завершено: " + timestamp(card.ClosedAt))
	}

	probes := append([]monitor.Probe(nil), card.Probes...)
	detailsTitle := "Сети и регионы"
	if closed {
		probes = card.Affected
		detailsTitle = "Сети, затронутые за время инцидента"
	} else {
		for _, affected := range card.Affected {
			if !slices.ContainsFunc(probes, func(probe monitor.Probe) bool {
				return probe.ID == affected.ID && probe.ASN == affected.ASN && probe.Region == affected.Region
			}) {
				affected.Verdicts = nil
				probes = append(probes, affected)
			}
		}
	}
	rows := groupProbes(probes)
	if len(rows) != 0 {
		fmt.Fprintf(&b, "<details><summary>%s</summary><table><tr><th>%s</th><th>%s</th><th>%s</th></tr>",
			inline(detailsTitle), inline("Сеть"), inline("Регион"), inline("Сканеры"))
		// Keep below Telegram's block and text limits even as the probe network grows.
		for _, row := range rows[:min(len(rows), 80)] {
			var statuses []string
			for _, item := range []struct {
				label string
				count int
			}{
				{"блокировка", row.blocked},
				{"доступен", row.healthy},
				{"неопределённо", row.unknown - row.missing},
				{"нет ответа", row.missing},
			} {
				if item.count > 0 {
					statuses = append(statuses, fmt.Sprintf("%s: %d", item.label, item.count))
				}
			}
			status := strings.Join(statuses, " · ")
			if closed {
				status = fmt.Sprintf("Блокировалось: %d", row.blocked+row.healthy+row.unknown)
			}
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>",
				inline(escape(row.network, 120)), inline(escape(row.region, 80)), inline(status))
		}
		b.WriteString("</table>")
		if len(rows) > 80 {
			paragraph(fmt.Sprintf("Показаны первые 80 из %d групп сетей.", len(rows)))
		}
		b.WriteString("</details>")
	}
	paragraph(`<a href="` + link + `">Cheburcheck</a>`)
	return b.String()
}

// A probe is healthy only when its only verdict is OK; other non-blocking results are uncertain.
func probeStatus(probe monitor.Probe) int {
	if slices.Contains(probe.Verdicts, "tspu_block") {
		return 0
	}
	if len(probe.Verdicts) == 1 && probe.Verdicts[0] == "ok" {
		return 2
	}
	return 1
}

type networkRow struct {
	network, region           string
	blocked, healthy, unknown int
	missing                   int
}

func groupProbes(probes []monitor.Probe) []networkRow {
	groups := make(map[[2]string]*networkRow)
	for _, probe := range probes {
		network := strings.TrimSpace(strings.Join([]string{probe.Provider, probe.ASN}, " "))
		if network == "" {
			network = "Сканер " + probe.ID
		}
		region := probe.Region
		if region == "" {
			region = "Не указан"
		}
		key := [2]string{network, region}
		row := groups[key]
		if row == nil {
			row = &networkRow{network: network, region: region}
			groups[key] = row
		}
		if len(probe.Verdicts) == 0 {
			row.missing++
		}
		switch probeStatus(probe) {
		case 0:
			row.blocked++
		case 1:
			row.unknown++
		default:
			row.healthy++
		}
	}
	rows := make([]networkRow, 0, len(groups))
	for _, row := range groups {
		rows = append(rows, *row)
	}
	rank := func(row networkRow) int {
		if row.blocked > 0 {
			return 0
		}
		if row.unknown > 0 {
			return 1
		}
		return 2
	}
	sort.Slice(rows, func(i, j int) bool {
		if rank(rows[i]) != rank(rows[j]) {
			return rank(rows[i]) < rank(rows[j])
		}
		if rows[i].network != rows[j].network {
			return rows[i].network < rows[j].network
		}
		return rows[i].region < rows[j].region
	})
	return rows
}

func escape(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit-1]) + "…"
	}
	return html.EscapeString(value)
}

func timestamp(at time.Time) string {
	if at.IsZero() {
		return "нет данных"
	}
	return at.In(moscow).Format("02.01, 15:04 МСК")
}

func duration(d time.Duration) string {
	minutes := int(max(d, 0) / time.Minute)
	if minutes < 1 {
		return "меньше минуты"
	}
	if minutes < 60 {
		return fmt.Sprintf("%d мин", minutes)
	}
	if minutes < 24*60 {
		if minutes%60 == 0 {
			return fmt.Sprintf("%d ч", minutes/60)
		}
		return fmt.Sprintf("%d ч %d мин", minutes/60, minutes%60)
	}
	if minutes/60%24 == 0 {
		return fmt.Sprintf("%d д", minutes/(24*60))
	}
	return fmt.Sprintf("%d д %d ч", minutes/(24*60), minutes/60%24)
}
