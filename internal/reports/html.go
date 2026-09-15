package reports

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"math"
	"strings"
	"time"
)

const chartWidth = 900.0

type htmlView struct {
	Report           Report
	Period           string
	PriceBasis       string
	DataThrough      string
	Cost             string
	CostDetail       string
	ProjectionLabel  string
	ProjectionCost   string
	ProjectionDetail string
	SpendChart       template.HTML
	RequestChart     template.HTML
	ModelChart       template.HTML
	TokenChart       template.HTML
	FailureChart     template.HTML
}

type dailyPoint struct {
	Day                         int
	Spend                       float64
	Requests, Success, Canceled int
	Failures                    int
}

type chartSlice struct {
	Name  string
	Value float64
	Class string
	Text  string
}

func HTML(r Report) ([]byte, error) {
	known := r.Metrics.Requests - r.Metrics.MissingCostRequests
	view := htmlView{
		Report: r, Period: reportPeriod(r.Start, r.Stop), PriceBasis: priceBasis(r.PricingSource),
		Cost:       estimatedCostValue(r.Metrics.KnownEstimatedCost, known, r.Metrics.Requests),
		CostDetail: pricedRequestDetail(known, r.Metrics.Requests),
		SpendChart: spendChart(r), RequestChart: requestChart(r),
		ModelChart: modelChart(r), TokenChart: tokenChart(r), FailureChart: failureChart(r),
	}
	if r.Projection != nil {
		view.DataThrough = "Data through " + displayDate(r.Projection.ObservedThrough)
		view.ProjectionLabel = "Projected " + projectionMonth(r.Projection.TargetMonth) + " cost"
		view.ProjectionCost = money(r.Projection.ProjectedKnownEstimatedCost)
		view.ProjectionDetail = fmt.Sprintf("Based on Sep 1–%d: %d projected requests", r.Projection.ElapsedDays, r.Projection.ProjectedRequests)
		if month := projectionMonth(r.Projection.TargetMonth); month != "" {
			view.ProjectionDetail = fmt.Sprintf("Based on %s 1–%d: %d projected requests", month, r.Projection.ElapsedDays, r.Projection.ProjectedRequests)
		}
	}
	var b bytes.Buffer
	if err := dashboard.Execute(&b, view); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func priceBasis(source string) string {
	if source == "current configuration (strict repricing)" {
		return "Current prices"
	}
	return "Prices when requests ran"
}

func reportPeriod(start, stop string) string {
	first, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return start + " to " + stop
	}
	last, err := time.Parse(time.RFC3339Nano, stop)
	if err != nil {
		return start + " to " + stop
	}
	last = last.Add(-time.Nanosecond).UTC()
	first = first.UTC()
	if first.Day() == 1 && first.Month() == last.Month() && first.Year() == last.Year() {
		return first.Format("January 2006")
	}
	if first.Year() == last.Year() && first.Month() == last.Month() {
		return fmt.Sprintf("%s %d–%d, %d", first.Format("Jan"), first.Day(), last.Day(), first.Year())
	}
	return fmt.Sprintf("%s %d, %d–%s %d, %d", first.Format("Jan"), first.Day(), first.Year(), last.Format("Jan"), last.Day(), last.Year())
}

func displayDate(value string) string {
	date, err := time.Parse("2006-01-02", value)
	if err != nil {
		return value
	}
	return date.Format("Jan 2")
}

func projectionMonth(value string) string {
	month, err := time.Parse("2006-01", value)
	if err != nil {
		return "month-end"
	}
	return month.Format("January")
}

func pricedRequestDetail(priced, requests int) string {
	if requests == 0 {
		return "No requests"
	}
	return fmt.Sprintf("Based on %d of %d requests", priced, requests)
}

func estimatedCostValue(value float64, priced, requests int) string {
	if requests == 0 || priced == 0 {
		return "unavailable"
	}
	return money(value)
}

func money(v float64) string {
	if v == 0 {
		return "$0.00"
	}
	if math.Abs(v) >= 1 {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("$%.6f", v)
}
func costDisplay(v float64, missing, total int) string {
	if total == 0 {
		return "n/a"
	}
	if missing >= total {
		return "unavailable"
	}
	if missing > 0 {
		return money(v) + " (partial)"
	}
	return money(v)
}
func coverage(known, total int) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(known)*100/float64(total))
}
func aliases(v Breakdown) string { return strings.Join(v.Aliases, ", ") }
func safe(value string) string   { return html.EscapeString(value) }
func chartTitle(title, description string) string {
	return fmt.Sprintf(`<title>%s</title><desc>%s</desc>`, safe(title), safe(description))
}
func emptyChart(title string) template.HTML {
	return template.HTML(fmt.Sprintf(`<section class="chart"><h2>%s</h2><p class="muted">No data in this report period.</p></section>`, safe(title)))
}

func monthDaily(r Report) []dailyPoint {
	if r.Projection == nil {
		return nil
	}
	values := make([]dailyPoint, r.Projection.ElapsedDays)
	for i := range values {
		values[i].Day = i + 1
	}
	for _, point := range r.Series {
		at, err := time.Parse(time.RFC3339, point.Start)
		if err != nil || at.UTC().Format("2006-01") != r.Projection.TargetMonth || at.Day() > len(values) {
			continue
		}
		entry := &values[at.Day()-1]
		entry.Spend += point.KnownEstimatedCost
		entry.Requests += point.Requests
		entry.Success += point.Successes
		entry.Canceled += point.Canceled
		entry.Failures += point.Failures
	}
	return values
}
func chartFrame(height float64, title, description string) *strings.Builder {
	b := &strings.Builder{}
	fmt.Fprintf(b, `<section class="chart"><h2>%s</h2><svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">%s`, safe(title), chartWidth, height, safe(description), chartTitle(title, description))
	return b
}
func finishChart(b *strings.Builder) template.HTML {
	b.WriteString(`</svg></section>`)
	return template.HTML(b.String())
}

func spendChart(r Report) template.HTML {
	if r.Projection == nil {
		return emptyChart("Estimated cost by day")
	}
	days := monthDaily(r)
	height, left, top, bottom := 280.0, 54.0, 24.0, 44.0
	plotW, plotH := chartWidth-left-34, height-top-bottom
	maxDaily, cumulative := 0.0, 0.0
	for _, d := range days {
		maxDaily = math.Max(maxDaily, d.Spend)
	}
	if maxDaily == 0 {
		maxDaily = 1
	}
	maxCumulative := math.Max(r.Projection.ProjectedKnownEstimatedCost, r.Projection.KnownEstimatedCost)
	if maxCumulative == 0 {
		maxCumulative = 1
	}
	b := chartFrame(height, "Estimated cost by day", "Daily estimated cost, total estimated cost so far, and projected month-end cost.")
	fmt.Fprintf(b, `<line class="axis" x1="%.0f" y1="%.0f" x2="%.0f" y2="%.0f"/><text class="axis-label" x="4" y="20">Estimated cost per day</text>`, left, top+plotH, chartWidth-34, top+plotH)
	barW := plotW / float64(max(1, r.Projection.DaysInMonth))
	points := make([]string, 0, len(days))
	cumulative = 0
	for i, d := range days {
		x := left + float64(i)*barW
		h := d.Spend / maxDaily * plotH
		fmt.Fprintf(b, `<rect class="spend-bar" x="%.2f" y="%.2f" width="%.2f" height="%.2f"><title>Day %d: %s estimated cost</title></rect>`, x+1, top+plotH-h, math.Max(1, barW-2), h, d.Day, money(d.Spend))
		cumulative += d.Spend
		cx, cy := x+barW/2, top+plotH-cumulative/maxCumulative*plotH
		points = append(points, fmt.Sprintf("%.2f,%.2f", cx, cy))
		if i == 0 || i == len(days)-1 || d.Day%7 == 0 {
			fmt.Fprintf(b, `<text class="tick" x="%.2f" y="%.0f">%d</text>`, cx, height-14, d.Day)
		}
	}
	fmt.Fprintf(b, `<polyline class="cumulative" points="%s"/>`, strings.Join(points, " "))
	if len(days) > 0 && r.Projection.DaysInMonth > r.Projection.ElapsedDays {
		x1 := left + (float64(r.Projection.ElapsedDays)-0.5)*barW
		x2 := left + plotW
		y1 := top + plotH - r.Projection.KnownEstimatedCost/maxCumulative*plotH
		y2 := top + plotH - r.Projection.ProjectedKnownEstimatedCost/maxCumulative*plotH
		fmt.Fprintf(b, `<line class="projection" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f"/><text class="projection-label" x="%.0f" y="%.0f">%s projected total</text>`, x1, y1, x2, y2, chartWidth-220, math.Max(18, y2-7), safe(money(r.Projection.ProjectedKnownEstimatedCost)))
	}
	fmt.Fprintf(b, `<text class="tick" x="%.0f" y="%.0f">%s</text><text class="tick" x="%.0f" y="%.0f">%s</text><text class="tick" x="%.0f" y="%.0f">%d</text><text class="axis-label" x="%.0f" y="%.0f">Day of month</text>`, 4.0, top+plotH, money(maxDaily), chartWidth-95, top+plotH, money(maxCumulative), left+plotW-barW/2, height-14, r.Projection.DaysInMonth, chartWidth/2-34, height-2)
	return finishChart(b)
}

func requestChart(r Report) template.HTML {
	days := monthDaily(r)
	if len(days) == 0 {
		return emptyChart("Requests by day")
	}
	height, left, top, bottom := 260.0, 54.0, 24.0, 42.0
	plotW, plotH := chartWidth-left-22, height-top-bottom
	maximum := 1
	for _, d := range days {
		maximum = max(maximum, d.Requests)
	}
	b := chartFrame(height, "Requests by day", "Daily requests split into successful, canceled, and failed outcomes.")
	fmt.Fprintf(b, `<line class="axis" x1="%.0f" y1="%.0f" x2="%.0f" y2="%.0f"/><text class="axis-label" x="4" y="20">Requests</text>`, left, top+plotH, chartWidth-22, top+plotH)
	barW := plotW / float64(len(days))
	for i, d := range days {
		x, y := left+float64(i)*barW+1, top+plotH
		for _, part := range []struct {
			value        int
			class, label string
		}{{d.Success, "outcome-success", "successful"}, {d.Canceled, "outcome-canceled", "canceled"}, {d.Failures, "outcome-failed", "failed"}} {
			h := float64(part.value) / float64(maximum) * plotH
			y -= h
			fmt.Fprintf(b, `<rect class="%s" x="%.2f" y="%.2f" width="%.2f" height="%.2f"><title>Day %d: %d %s</title></rect>`, part.class, x, y, math.Max(1, barW-2), h, d.Day, part.value, part.label)
		}
		if i == 0 || i == len(days)-1 || d.Day%7 == 0 {
			fmt.Fprintf(b, `<text class="tick" x="%.2f" y="%.0f">%d</text>`, x+barW/2, height-14, d.Day)
		}
	}
	fmt.Fprintf(b, `<text class="tick" x="4" y="%.0f">%d</text><g class="legend"><rect class="outcome-success" x="%.0f" y="6" width="10" height="10"/><text x="%.0f" y="16">Successful</text><rect class="outcome-canceled" x="%.0f" y="6" width="10" height="10"/><text x="%.0f" y="16">Canceled</text><rect class="outcome-failed" x="%.0f" y="6" width="10" height="10"/><text x="%.0f" y="16">Failed</text></g>`, top+plotH, maximum, chartWidth-300, chartWidth-285, chartWidth-205, chartWidth-190, chartWidth-115, chartWidth-100)
	return finishChart(b)
}

func donutChart(title, description, totalText string, slices []chartSlice) template.HTML {
	var total float64
	for _, slice := range slices {
		total += slice.Value
	}
	if total == 0 {
		return emptyChart(title)
	}
	const centerX, centerY, radius = 180.0, 120.0, 78.0
	var b strings.Builder
	b.WriteString(`<section class="chart donut"><h2>` + safe(title) + `</h2><svg viewBox="0 0 360 250" role="img" aria-label="` + safe(description) + `">` + chartTitle(title, description))
	circumference, offset := 2*math.Pi*radius, 0.0
	for _, slice := range slices {
		if slice.Value <= 0 {
			continue
		}
		dash := slice.Value / total * circumference
		fmt.Fprintf(&b, `<circle class="donut-ring %s" cx="%.0f" cy="%.0f" r="%.0f" pathLength="%.5f" stroke-dasharray="%.5f %.5f" stroke-dashoffset="%.5f"><title>%s: %s</title></circle>`, slice.Class, centerX, centerY, radius, circumference, dash, circumference-dash, -offset, safe(slice.Name), safe(slice.Text))
		offset += dash
	}
	fmt.Fprintf(&b, `<circle class="donut-hole" cx="%.0f" cy="%.0f" r="52"/><text class="donut-total" x="%.0f" y="%.0f">Total</text><text class="donut-value" x="%.0f" y="%.0f">%s</text></svg><div class="donut-legend">`, centerX, centerY, centerX, centerY-8, centerX, centerY+14, safe(totalText))
	for _, slice := range slices {
		if slice.Value <= 0 {
			continue
		}
		fmt.Fprintf(&b, `<div class="donut-legend-item"><span class="legend-swatch %s"></span><span>%s</span><strong>%s</strong></div>`, slice.Class, safe(slice.Name), safe(slice.Text))
	}
	b.WriteString(`</div></section>`)
	return template.HTML(b.String())
}
func modelChart(r Report) template.HTML {
	slices := make([]chartSlice, 0, len(r.Models))
	for i, m := range r.Models {
		slices = append(slices, chartSlice{Name: label(m.CurrentAlias, m.Name), Value: m.KnownEstimatedCost, Class: fmt.Sprintf("slice-%d", i%6), Text: money(m.KnownEstimatedCost)})
	}
	return donutChart("Estimated cost by model", "Estimated cost divided by model.", money(r.Metrics.KnownEstimatedCost), slices)
}
func tokenChart(r Report) template.HTML {
	m := r.Metrics
	uncached := m.KnownInputTokens - m.KnownCacheReadInputTokens - m.KnownCacheWriteInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return donutChart("Tokens by type", "Input, cache, and output tokens across the report.", formatTokens(uncached+m.KnownCacheReadInputTokens+m.KnownCacheWriteInputTokens+m.KnownOutputTokens), []chartSlice{{"Uncached input", float64(uncached), "slice-0", formatTokens(uncached)}, {"Cache read", float64(m.KnownCacheReadInputTokens), "slice-1", formatTokens(m.KnownCacheReadInputTokens)}, {"Cache write", float64(m.KnownCacheWriteInputTokens), "slice-2", formatTokens(m.KnownCacheWriteInputTokens)}, {"Output", float64(m.KnownOutputTokens), "slice-3", formatTokens(m.KnownOutputTokens)}})
}
func failureChart(r Report) template.HTML {
	if len(r.FailureBreakdowns) == 0 {
		return emptyChart("Failed requests by cause")
	}
	height := float64(74 + len(r.FailureBreakdowns)*30)
	maxValue := 1
	for _, v := range r.FailureBreakdowns {
		maxValue = max(maxValue, v.Requests)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<section class="chart"><h2>Failed requests by cause</h2><svg viewBox="0 0 900 %.0f" role="img" aria-label="Failure counts by category and endpoint.">%s`, height, chartTitle("Failed requests by cause", "Failed request counts by cause and endpoint."))
	for i, v := range r.FailureBreakdowns {
		y := 32 + float64(i)*30
		name := fmt.Sprintf("%s · %s", v.Category, v.Endpoint)
		if v.HTTPStatus > 0 {
			name += fmt.Sprintf(" · HTTP %d", v.HTTPStatus)
		}
		w := float64(v.Requests) / float64(maxValue) * 440
		fmt.Fprintf(&b, `<text class="failure-label" x="0" y="%.0f">%s</text><rect class="failure-bar" x="420" y="%.0f" width="%.2f" height="16"><title>%s: %d requests</title></rect><text class="legend-value" x="%.0f" y="%.0f">%d</text>`, y, safe(name), y-13, w, safe(name), v.Requests, 430+w, y, v.Requests)
	}
	b.WriteString(`</svg></section>`)
	return template.HTML(b.String())
}
func formatTokens(v int64) string { return fmt.Sprintf("%d", v) }

var dashboard = template.Must(template.New("report").Funcs(template.FuncMap{"cost": func(v Breakdown) string { return costDisplay(v.KnownEstimatedCost, v.MissingCostRequests, v.Requests) }, "aliases": aliases}).Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Bedrock Local Proxy usage report</title><style>:root{--ink:#172033;--muted:#637086;--line:#dce2ea;--panel:#fff;--bg:#f4f6f9;--blue:#346ae6;--green:#16845b;--amber:#bd7600;--red:#c44750;--purple:#7655c7;--teal:#008e9b}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 system-ui,-apple-system,sans-serif}.shell{max-width:1180px;margin:auto;padding:34px 24px 54px}h1{font-size:32px;margin:0 0 4px}h2{font-size:18px;margin:0 0 12px}.muted{color:var(--muted)}.summary{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin:24px 0}.card,.chart,.table-wrap{background:var(--panel);border:1px solid var(--line);border-radius:10px}.card{padding:16px}.card b{font-size:24px;display:block;margin-top:4px}.charts{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px}.chart{padding:16px;overflow:hidden}.chart:first-child,.chart:nth-child(2),.chart:last-child{grid-column:1/-1}.chart svg{width:100%;height:auto;display:block}.axis{stroke:#b8c3d2;stroke-width:1}.axis-label,.tick{fill:var(--muted);font-size:11px}.spend-bar{fill:var(--blue);opacity:.62}.cumulative{fill:none;stroke:var(--green);stroke-width:3}.projection{stroke:var(--green);stroke-width:2;stroke-dasharray:7 5}.projection-label{fill:var(--green);font-size:12px;font-weight:600}.outcome-success,.slice-0{fill:var(--blue);stroke:var(--blue)}.outcome-canceled,.slice-1{fill:var(--amber);stroke:var(--amber)}.outcome-failed,.slice-2{fill:var(--red);stroke:var(--red)}.slice-3{fill:var(--purple);stroke:var(--purple)}.slice-4{fill:var(--teal);stroke:var(--teal)}.slice-5{fill:#8b6474;stroke:#8b6474}.legend text,.legend-text,.legend-value{fill:var(--muted);font-size:12px}.legend-value{text-anchor:end}.donut-ring{fill:none;stroke-width:38;transform:rotate(-90deg);transform-origin:180px 120px}.donut-hole{fill:var(--panel)}.donut-total{font-size:11px;fill:var(--muted);text-anchor:middle}.donut-value{font-size:15px;fill:var(--ink);font-weight:600;text-anchor:middle}.donut svg{max-width:360px;margin:auto}.donut-legend{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px 18px;margin:10px 0 2px}.donut-legend-item{display:grid;grid-template-columns:12px minmax(0,1fr) auto;gap:7px;align-items:center;color:var(--muted)}.donut-legend-item strong{color:var(--ink);font-weight:600}.legend-swatch{width:12px;height:12px;display:block}.legend-swatch.slice-0{background:var(--blue)}.legend-swatch.slice-1{background:var(--amber)}.legend-swatch.slice-2{background:var(--red)}.legend-swatch.slice-3{background:var(--purple)}.legend-swatch.slice-4{background:var(--teal)}.legend-swatch.slice-5{background:#8b6474}.failure-bar{fill:var(--red);opacity:.75}.failure-label{fill:var(--ink);font-size:12px}.table-wrap{overflow:auto;margin-top:18px}table{border-collapse:collapse;width:100%;min-width:720px}th,td{padding:10px;text-align:right;border-bottom:1px solid var(--line);white-space:nowrap}th:first-child,td:first-child{text-align:left}th{color:var(--muted);font-size:11px;text-transform:uppercase}tbody tr:last-child td{border:0}@media(max-width:720px){.summary,.charts,.donut-legend{grid-template-columns:1fr}.chart:first-child,.chart:nth-child(2),.chart:last-child{grid-column:auto}.shell{padding:24px 14px}.card b{font-size:21px}}</style></head><body><main class="shell"><h1>Usage report</h1><p class="muted">{{.Period}} · {{.PriceBasis}}{{if .DataThrough}} · {{.DataThrough}}{{end}}</p><div class="summary"><div class="card">Requests<b>{{.Report.Metrics.Requests}}</b></div><div class="card">Request outcomes<b>{{.Report.Metrics.Successes}} successful</b><span class="muted">{{.Report.Metrics.Canceled}} canceled · {{.Report.Metrics.Failures}} failed</span></div><div class="card">Estimated cost so far<b>{{.Cost}}</b><span class="muted">{{.CostDetail}}</span></div><div class="card">{{if .ProjectionCost}}{{.ProjectionLabel}}{{else}}Projected month-end cost{{end}}<b>{{if .ProjectionCost}}{{.ProjectionCost}}{{else}}n/a{{end}}</b><span class="muted">{{if .ProjectionCost}}{{.ProjectionDetail}}{{else}}No request data{{end}}</span></div></div><div class="charts">{{.SpendChart}}{{.RequestChart}}{{.ModelChart}}{{.TokenChart}}{{.FailureChart}}</div><section><h2>Models</h2><div class="table-wrap"><table><tr><th>Upstream model</th><th>Current alias</th><th>Historical aliases</th><th>Requests</th><th>Input</th><th>Output</th><th>Estimated cost</th></tr>{{range .Report.Models}}<tr><td>{{.Name}}</td><td>{{.CurrentAlias}}</td><td>{{aliases .}}</td><td>{{.Requests}}</td><td>{{.KnownInputTokens}}</td><td>{{.KnownOutputTokens}}</td><td>{{cost .}}</td></tr>{{end}}</table></div></section><section><h2>Endpoints</h2><div class="table-wrap"><table><tr><th>Endpoint</th><th>Requests</th><th>Successful</th><th>Canceled</th><th>Failed</th><th>Estimated cost</th></tr>{{range .Report.Endpoints}}<tr><td>{{.Name}}</td><td>{{.Requests}}</td><td>{{.Successes}}</td><td>{{.Canceled}}</td><td>{{.Failures}}</td><td>{{cost .}}</td></tr>{{end}}</table></div></section><section><h2>Session tags</h2><div class="table-wrap"><table><tr><th>Tag</th><th>Requests</th><th>Estimated cost</th></tr>{{range .Report.SessionTags}}<tr><td>{{.Name}}</td><td>{{.Requests}}</td><td>{{cost .}}</td></tr>{{end}}</table></div></section><section><h2>Usage warnings</h2><div class="table-wrap"><table><tr><th>Path</th><th>Requests</th></tr>{{range .Report.UsageWarnings}}<tr><td>{{.Path}}</td><td>{{.Requests}}</td></tr>{{end}}</table></div></section><section><h2>Pricing blockers</h2><div class="table-wrap"><table><tr><th>Model</th><th>Reason</th><th>Requests</th></tr>{{range .Report.PricingIssues}}<tr><td>{{.Model}}</td><td>{{.Reason}}</td><td>{{.Requests}}</td></tr>{{end}}</table></div></section></main></body></html>`))
