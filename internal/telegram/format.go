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
		return "<h3>🔴 Блокировка распространилась на все проверенные сети</h3><p><b>" + host +
			"</b> · " + address + "</p><p>Проверено: " + timestamp(card.CheckedAt) +
			`</p><p><a href="` + link + `">Открыть Cheburcheck</a></p>`
	}
	if event.Kind == monitor.EventSummary {
		heading := "🟢 Восстановлено"
		text := "Блокировка больше не подтверждается затронутыми сканерами."
		if card.Stopped {
			heading = "⚪ Наблюдение прекращено"
			text = "Цель больше не отслеживается. Восстановление не подтверждено."
		}
		return "<h3>" + heading + " · " + host + "</h3><p>" + address + "</p><p>" + text +
			"</p><p>Период наблюдения: " + timestamp(card.StartedAt) + " — " + timestamp(end) +
			"</p><p>Длительность: " + duration(end.Sub(card.StartedAt)) + "</p>"
	}

	var blocked, healthy, unknown int
	for _, probe := range card.Probes {
		switch probeStatus(probe) {
		case 0:
			blocked++
		case 1:
			unknown++
		default:
			healthy++
		}
	}
	unknown += max(0, card.Online-len(card.Probes))

	closed := !card.ClosedAt.IsZero()
	heading := "🟠 Восстановление не подтверждено"
	if blocked > 0 {
		heading = "🔴 ТСПУ-блокировка"
		if healthy > 0 {
			heading = "🟠 Частичная ТСПУ-блокировка"
		} else if blocked == len(card.Probes) {
			heading = "🔴 ТСПУ у всех ответивших"
		}
	}
	switch {
	case card.Stopped:
		heading = "⚪ Наблюдение прекращено"
	case closed:
		heading = "🟢 Восстановлено"
	case card.Unavailable:
		heading = "⚪ Нет свежих данных · ТСПУ-инцидент"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<h3>%s · %s</h3>", heading, host)
	// Strikethrough stays inside each block: wrapping a table or details in <s> is invalid.
	inline := func(text string) string {
		if closed {
			return "<s>" + text + "</s>"
		}
		return text
	}
	paragraph := func(text string) { fmt.Fprintf(&b, "<p>%s</p>", inline(text)) }
	paragraph(address)
	if card.Stopped {
		paragraph("Цель больше не отслеживается. Восстановление не подтверждено.")
	} else if card.Unavailable {
		paragraph("Проверка недоступна. Ниже — данные последней успешной проверки.")
	}
	paragraph(fmt.Sprintf("Ответили <b>%d из %d</b> сканеров · 🔴 ТСПУ: <b>%d</b> · 🟢 OK: <b>%d</b> · ⚪ Неопределённо: <b>%d</b>",
		len(card.Probes), card.Online, blocked, healthy, unknown))
	paragraph("Первое обнаружение: " + timestamp(card.StartedAt))
	paragraph("Период наблюдения: " + duration(end.Sub(card.StartedAt)))
	checked := "Проверено: "
	if card.Unavailable || card.Stopped {
		checked = "Последняя успешная проверка: "
	}
	paragraph(checked + timestamp(card.CheckedAt))
	if closed {
		paragraph("Наблюдение завершено: " + timestamp(card.ClosedAt))
	}

	probes := append([]monitor.Probe(nil), card.Probes...)
	detailsTitle := "Результаты по сетям"
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
			status := fmt.Sprintf("🔴 %d · 🟢 %d · ⚪ %d", row.blocked, row.healthy, row.unknown)
			if row.missing > 0 {
				status += fmt.Sprintf(" · Нет свежих данных: %d", row.missing)
			}
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
	paragraph(`<a href="` + link + `">Открыть Cheburcheck</a>`)
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
	return at.In(moscow).Format("02.01.2006 15:04 МСК")
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
		return fmt.Sprintf("%d ч %d мин", minutes/60, minutes%60)
	}
	return fmt.Sprintf("%d д %d ч %d мин", minutes/(24*60), minutes/60%24, minutes%60)
}
