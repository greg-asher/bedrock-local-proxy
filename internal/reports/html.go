package reports

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

type htmlView struct {
	Report           Report
	PeriodStart      string
	PeriodStop       string
	GeneratedAt      string
	SuccessRate      string
	CostSummary      string
	CostCoverage     string
	UsageCoverage    string
	AverageKnownCost string
	SessionsSummary  string
	ContextUsage     string
	OutputUsage      string
	RequestBars      []requestBar
	CostBars         []costBar
	RequestScale     string
	CostScale        string
	FirstBucket      string
	LastBucket       string
	Notices          []htmlNotice
	HasRequests      bool
	HasCompatibility bool
}

type htmlNotice struct {
	Tone  string
	Title string
	Text  string
}

type requestBar struct {
	Label         string
	Value         string
	SuccessHeight int
	FailureHeight int
}

type costBar struct {
	Label  string
	Value  string
	Height int
	Class  string
}

// HTML renders a private, standalone dashboard with no external assets.
func HTML(report Report) ([]byte, error) {
	metrics := report.Metrics
	knownCosts := metrics.Requests - metrics.MissingCostRequests
	view := htmlView{
		Report:           report,
		PeriodStart:      displayTimestamp(report.Start),
		PeriodStop:       displayTimestamp(report.Stop),
		GeneratedAt:      displayTimestamp(report.GeneratedAt),
		SuccessRate:      ratio(metrics.Successes, metrics.Requests),
		CostSummary:      costDisplay(metrics.KnownEstimatedCost, metrics.MissingCostRequests, metrics.Requests),
		CostCoverage:     coverage(knownCosts, metrics.Requests),
		UsageCoverage:    coverage(metrics.Requests-metrics.MissingUsageRequests, metrics.Requests),
		AverageKnownCost: averageKnownCost(metrics.KnownEstimatedCost, knownCosts),
		SessionsSummary:  plural(report.Sessions, "session", "sessions"),
		ContextUsage:     utilization(metrics.AverageContextUtilization, metrics.ContextUtilizationSamples),
		OutputUsage:      utilization(metrics.AverageOutputUtilization, metrics.OutputUtilizationSamples),
		HasRequests:      metrics.Requests > 0,
		HasCompatibility: len(report.MetadataProfiles)+len(report.Catalogs)+len(report.ClaudeSettings) > 0,
	}
	view.RequestBars, view.RequestScale = buildRequestBars(report.Series)
	view.CostBars, view.CostScale = buildCostBars(report.Series)
	if len(report.Series) > 0 {
		view.FirstBucket = displayBucket(report.Series[0].Start)
		view.LastBucket = displayBucket(report.Series[len(report.Series)-1].Start)
	}
	view.Notices = buildNotices(report)

	var output bytes.Buffer
	if err := dashboardTemplate.Execute(&output, view); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func buildNotices(report Report) []htmlNotice {
	metrics := report.Metrics
	var result []htmlNotice
	if metrics.Requests == 0 {
		return []htmlNotice{{Tone: "info", Title: "No generation requests", Text: "No generation events were recorded inside this time window."}}
	}
	if metrics.MissingCostRequests > 0 {
		result = append(result, htmlNotice{Tone: "warning", Title: "Cost coverage is incomplete", Text: fmt.Sprintf("%s of %s have no estimate. Check token usage and all applicable input, output, cache-read, and cache-write prices.", plural(metrics.MissingCostRequests, "request", "requests"), plural(metrics.Requests, "request", "requests"))})
	}
	if metrics.MissingUsageRequests > 0 {
		result = append(result, htmlNotice{Tone: "warning", Title: "Usage coverage is incomplete", Text: fmt.Sprintf("%s did not include complete input and output token counts.", plural(metrics.MissingUsageRequests, "request", "requests"))})
	}
	if metrics.Failures > 0 {
		result = append(result, htmlNotice{Tone: "danger", Title: "Requests need attention", Text: fmt.Sprintf("%s failed, including %s. Use the endpoint and client sections to narrow the source.", plural(metrics.Failures, "request", "requests"), plural(metrics.Canceled, "canceled request", "canceled requests"))})
	}
	if report.SkippedSessions > 0 {
		result = append(result, htmlNotice{Tone: "warning", Title: "Some sessions were skipped", Text: fmt.Sprintf("%s unreadable or did not match the report schema.", plural(report.SkippedSessions, "session directory was", "session directories were"))})
	}
	if len(result) == 0 {
		result = append(result, htmlNotice{Tone: "success", Title: "Complete report coverage", Text: "Every generation request has usage and cost data, and no request failures were recorded."})
	}
	return result
}

func buildRequestBars(points []Point) ([]requestBar, string) {
	maximum := 0
	for _, point := range points {
		if point.Requests > maximum {
			maximum = point.Requests
		}
	}
	result := make([]requestBar, 0, len(points))
	for _, point := range points {
		totalHeight := scaleHeight(float64(point.Requests), float64(maximum))
		failureHeight := 0
		if point.Requests > 0 {
			failureHeight = int(math.Round(float64(point.Failures) / float64(point.Requests) * float64(totalHeight)))
		}
		result = append(result, requestBar{
			Label:         point.Start,
			Value:         fmt.Sprintf("%s requests; %s successful; %s failed", formatInt(point.Requests), formatInt(point.Successes), formatInt(point.Failures)),
			SuccessHeight: totalHeight - failureHeight,
			FailureHeight: failureHeight,
		})
	}
	return result, formatInt(maximum)
}

func buildCostBars(points []Point) ([]costBar, string) {
	maximum := 0.0
	for _, point := range points {
		maximum = math.Max(maximum, point.KnownEstimatedCost)
	}
	result := make([]costBar, 0, len(points))
	for _, point := range points {
		class := "complete"
		if point.MissingCostRequests >= point.Requests && point.Requests > 0 {
			class = "missing"
		} else if point.MissingCostRequests > 0 {
			class = "partial"
		}
		height := scaleHeight(point.KnownEstimatedCost, maximum)
		if height == 0 && point.Requests > point.MissingCostRequests {
			height = 3
		}
		result = append(result, costBar{Label: point.Start, Value: costDisplay(point.KnownEstimatedCost, point.MissingCostRequests, point.Requests), Height: height, Class: class})
	}
	return result, money(maximum)
}

func scaleHeight(value, maximum float64) int {
	if value <= 0 || maximum <= 0 {
		return 0
	}
	return max(3, int(math.Round(value/maximum*152)))
}

func costDisplay(known float64, missing, requests int) string {
	if requests == 0 {
		return "n/a"
	}
	if missing >= requests {
		return "unavailable"
	}
	value := money(known)
	if missing > 0 {
		return value + " partial"
	}
	return value
}

func averageKnownCost(cost float64, requests int) string {
	if requests <= 0 {
		return "n/a"
	}
	return money(cost / float64(requests))
}

func coverage(known, total int) string {
	if total <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(max(known, 0))*100/float64(total))
}

func ratio(value, total int) string {
	if total <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(value)*100/float64(total))
}

func utilization(value float64, samples int) string {
	if samples == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", value*100)
}

func money(value float64) string {
	switch {
	case value == 0:
		return "$0.00"
	case math.Abs(value) >= 100:
		return fmt.Sprintf("$%.0f", value)
	case math.Abs(value) >= 1:
		return fmt.Sprintf("$%.2f", value)
	case math.Abs(value) >= 0.01:
		return fmt.Sprintf("$%.3f", value)
	default:
		return fmt.Sprintf("$%.6f", value)
	}
}

func formatInt(value int) string { return comma(strconv.Itoa(value)) }

func plural(value int, singular, plural string) string {
	label := plural
	if value == 1 {
		label = singular
	}
	return formatInt(value) + " " + label
}

func formatTokens(value int64) string { return comma(strconv.FormatInt(value, 10)) }

func comma(value string) string {
	start := 0
	if strings.HasPrefix(value, "-") {
		start = 1
	}
	for position := len(value) - 3; position > start; position -= 3 {
		value = value[:position] + "," + value[position:]
	}
	return value
}

func displayTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("Jan 2, 2006 15:04 UTC")
}

func displayBucket(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("Jan 2 15:04")
}

func shortID(value string) string {
	if len(value) <= 28 {
		return value
	}
	return value[:12] + "…" + value[len(value)-8:]
}

func breakdownCost(value Breakdown) string {
	return costDisplay(value.KnownEstimatedCost, value.MissingCostRequests, value.Requests)
}

func breakdownAverage(value Breakdown) string {
	return averageKnownCost(value.KnownEstimatedCost, value.Requests-value.MissingCostRequests)
}

func breakdownShare(value Breakdown, total float64) string {
	if total <= 0 || value.Requests <= value.MissingCostRequests {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", value.KnownEstimatedCost*100/total)
}

var dashboardTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"requests":   formatInt,
	"tokens":     formatTokens,
	"rowSuccess": func(value Breakdown) string { return ratio(value.Successes, value.Requests) },
	"rowCoverage": func(value Breakdown) string {
		return coverage(value.Requests-value.MissingCostRequests, value.Requests)
	},
	"rowCost":    breakdownCost,
	"rowAverage": breakdownAverage,
	"rowShare":   breakdownShare,
	"shortID":    shortID,
	"money":      money,
	"dict":       func(rows []Breakdown, total float64) []any { return []any{rows, total} },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Bedrock Local Proxy usage report</title><style>
:root{color-scheme:light;--ink:#172033;--muted:#687386;--line:#dfe4eb;--panel:#fff;--bg:#f3f5f8;--blue:#356ae6;--blue-soft:#eaf0ff;--green:#16845b;--green-soft:#e8f6f0;--amber:#a45c00;--amber-soft:#fff5df;--red:#b53b42;--red-soft:#ffedef;--shadow:0 10px 28px rgba(23,32,51,.06)}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.45 Inter,ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;font-variant-numeric:tabular-nums}.shell{max-width:1240px;margin:0 auto;padding:36px 24px 56px}.eyebrow{color:var(--blue);font-size:12px;font-weight:750;letter-spacing:.11em;text-transform:uppercase}h1{font-size:34px;line-height:1.12;letter-spacing:-.035em;margin:6px 0 8px}h2{font-size:18px;letter-spacing:-.015em;margin:0}.period,.muted{color:var(--muted)}.period{font-size:15px}.summary{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin:24px 0}.metric,.panel,.notice{background:var(--panel);border:1px solid var(--line);border-radius:12px;box-shadow:var(--shadow)}.metric{padding:17px 18px;min-height:112px}.metric .label{color:var(--muted);font-size:12px;font-weight:700;letter-spacing:.045em;text-transform:uppercase}.metric strong{display:block;font-size:27px;letter-spacing:-.025em;margin:7px 0 2px}.metric small{color:var(--muted)}.notices{display:grid;gap:8px;margin:0 0 20px}.notice{padding:12px 15px;display:flex;gap:12px;box-shadow:none}.notice:before{content:"";width:4px;border-radius:4px;background:var(--blue);flex:0 0 auto}.notice.warning:before{background:var(--amber)}.notice.danger:before{background:var(--red)}.notice.success:before{background:var(--green)}.notice b{display:block;margin-bottom:1px}.notice p{margin:0;color:var(--muted)}.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px;margin:14px 0}.panel{padding:18px;overflow:hidden}.panel-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:12px}.panel-head p{margin:3px 0 0;color:var(--muted)}.pill{border-radius:999px;background:var(--blue-soft);color:#214da7;padding:4px 9px;font-size:12px;font-weight:700;white-space:nowrap}.pill.green{background:var(--green-soft);color:#116342}.plot{height:184px;display:grid;grid-template-columns:44px minmax(0,1fr);gap:8px}.y-axis{height:160px;display:flex;flex-direction:column;justify-content:space-between;color:var(--muted);font-size:11px;text-align:right}.columns{height:160px;border-bottom:1px solid #bfc8d5;background:repeating-linear-gradient(to bottom,transparent 0,transparent 39px,#edf0f4 40px);display:flex;align-items:flex-end;gap:3px;padding:0 3px}.column{min-width:3px;flex:1;display:flex;flex-direction:column;justify-content:flex-end;height:160px}.segment.success{background:var(--blue)}.segment.failure{background:var(--red)}.segment.cost{background:var(--green);border-radius:3px 3px 0 0}.segment.cost.partial{background:var(--amber)}.segment.cost.missing{height:3px!important;background:repeating-linear-gradient(90deg,var(--red) 0,var(--red) 3px,transparent 3px,transparent 6px)}.x-axis{margin-left:52px;display:flex;justify-content:space-between;color:var(--muted);font-size:11px}.legend{display:flex;gap:14px;color:var(--muted);font-size:12px;margin-top:8px}.key:before{content:"";display:inline-block;width:9px;height:9px;border-radius:2px;margin-right:5px;background:var(--blue)}.key.failure:before{background:var(--red)}.key.cost:before{background:var(--green)}.key.partial:before{background:var(--amber)}.section{margin-top:18px}.section>.panel-head{padding:0 2px}.table-wrap{overflow:auto;border:1px solid var(--line);border-radius:10px}table{border-collapse:collapse;width:100%;min-width:820px;background:#fff}th,td{padding:10px 12px;text-align:right;border-bottom:1px solid #edf0f4;white-space:nowrap}th:first-child,td:first-child{text-align:left;position:sticky;left:0;background:inherit}th{color:var(--muted);font-size:11px;letter-spacing:.04em;text-transform:uppercase;background:#f8f9fb}tbody tr:last-child td{border-bottom:0}tbody tr:hover{background:#f8faff}.primary{font-weight:700}.compact table{min-width:520px}.diagnostics{margin-top:18px}.diagnostics summary{cursor:pointer;font-weight:700;font-size:16px;padding:16px 18px}.diagnostics[open] summary{border-bottom:1px solid var(--line)}.diagnostic-grid{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px;padding:14px}.diagnostic-grid .table-wrap{min-width:0}.diagnostic-grid table{min-width:0}.empty{height:184px;display:grid;place-items:center;color:var(--muted)}footer{border-top:1px solid var(--line);color:var(--muted);font-size:12px;margin-top:24px;padding-top:16px;overflow-wrap:anywhere}code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px}@media(max-width:900px){.summary{grid-template-columns:repeat(2,minmax(0,1fr))}.grid,.diagnostic-grid{grid-template-columns:1fr}}@media(max-width:560px){.shell{padding:24px 14px 40px}.summary{grid-template-columns:1fr 1fr}.metric{min-height:100px;padding:14px}.metric strong{font-size:22px}h1{font-size:28px}.panel{padding:14px}.grid{grid-template-columns:1fr}}@media print{body{background:#fff}.shell{max-width:none;padding:0}.metric,.panel,.notice{box-shadow:none;break-inside:avoid}.diagnostics{display:none}}
</style></head><body><main class="shell">
<header><div class="eyebrow">Bedrock Local Proxy</div><h1>Usage report</h1><div class="period">{{.PeriodStart}} → {{.PeriodStop}}</div></header>
<section class="summary" aria-label="Report summary">
<div class="metric"><span class="label">Estimated cost</span><strong>{{.CostSummary}}</strong><small>{{.CostCoverage}} cost coverage</small></div>
<div class="metric"><span class="label">Generation requests</span><strong>{{requests .Report.Metrics.Requests}}</strong><small>{{.SuccessRate}} successful · {{.SessionsSummary}}</small></div>
<div class="metric"><span class="label">Reported tokens</span><strong>{{tokens .Report.Metrics.KnownInputTokens}} / {{tokens .Report.Metrics.KnownOutputTokens}}</strong><small>input / output · {{.UsageCoverage}} usage coverage</small></div>
<div class="metric"><span class="label">Prompt cache</span><strong>{{tokens .Report.Metrics.KnownCacheReadInputTokens}} / {{tokens .Report.Metrics.KnownCacheWriteInputTokens}}</strong><small>read / write tokens</small></div>
<div class="metric"><span class="label">Average known cost</span><strong>{{.AverageKnownCost}}</strong><small>per request with an estimate</small></div>
<div class="metric"><span class="label">Average utilization</span><strong>{{.ContextUsage}} / {{.OutputUsage}}</strong><small>context / output ceiling</small></div>
<div class="metric"><span class="label">Function calls</span><strong>{{requests .Report.Metrics.FunctionToolCalls}}</strong><small>{{requests .Report.Metrics.UnsupportedFeatureRejections}} unsupported feature rejections</small></div>
<div class="metric"><span class="label">Other activity</span><strong>{{requests .Report.Metrics.ModelListEvents}}</strong><small>model-list events excluded from totals</small></div>
</section>
<section class="notices" aria-label="Report findings">{{range .Notices}}<div class="notice {{.Tone}}"><div><b>{{.Title}}</b><p>{{.Text}}</p></div></div>{{end}}</section>
<section class="grid" aria-label="Trends">
<div class="panel"><div class="panel-head"><div><h2>Request activity</h2><p>Successful and failed generations by time bucket</p></div><span class="pill">{{requests .Report.Metrics.Requests}} total</span></div>{{if .HasRequests}}<div class="plot"><div class="y-axis"><span>{{.RequestScale}}</span><span>0</span></div><div class="columns">{{range .RequestBars}}<div class="column" title="{{.Label}} · {{.Value}}"><div class="segment success" style="height:{{.SuccessHeight}}px"></div><div class="segment failure" style="height:{{.FailureHeight}}px"></div></div>{{end}}</div></div><div class="x-axis"><span>{{.FirstBucket}}</span><span>UTC</span><span>{{.LastBucket}}</span></div><div class="legend"><span class="key">Successful</span><span class="key failure">Failed or canceled</span></div>{{else}}<div class="empty">No request activity in this period</div>{{end}}</div>
<div class="panel"><div class="panel-head"><div><h2>Estimated spend</h2><p>Recorded estimates by time bucket</p></div><span class="pill green">{{.CostSummary}}</span></div>{{if .HasRequests}}<div class="plot"><div class="y-axis"><span>{{.CostScale}}</span><span>$0</span></div><div class="columns">{{range .CostBars}}<div class="column" title="{{.Label}} · {{.Value}}"><div class="segment cost {{.Class}}" style="height:{{.Height}}px"></div></div>{{end}}</div></div><div class="x-axis"><span>{{.FirstBucket}}</span><span>UTC</span><span>{{.LastBucket}}</span></div><div class="legend"><span class="key cost">Complete</span><span class="key partial">Partial coverage</span><span class="key failure">Unavailable</span></div>{{else}}<div class="empty">No estimated spend in this period</div>{{end}}</div>
</section>
<section class="section"><div class="panel-head"><div><h2>Cost and usage by model</h2><p>Ranked by known estimated cost</p></div></div>{{template "usage-table" (dict .Report.Models .Report.Metrics.KnownEstimatedCost)}}</section>
<section class="section"><div class="panel-head"><div><h2>Cost and usage by session tag</h2><p>Use tags to attribute runs to a project, workflow, or test</p></div></div>{{template "usage-table" (dict .Report.SessionTags .Report.Metrics.KnownEstimatedCost)}}</section>
<section class="grid"><div class="panel compact"><div class="panel-head"><div><h2>Endpoints</h2><p>Protocol traffic and outcomes</p></div></div>{{template "compact-table" .Report.Endpoints}}</div><div class="panel compact"><div class="panel-head"><div><h2>Clients</h2><p>Observed client family and version</p></div></div>{{template "compact-table" .Report.Clients}}</div></section>
{{if .HasCompatibility}}<details class="panel diagnostics"><summary>Compatibility details</summary><div class="diagnostic-grid"><div><h2>Metadata profiles</h2>{{template "diagnostic-table" .Report.MetadataProfiles}}</div><div><h2>Codex catalogs</h2>{{template "id-table" .Report.Catalogs}}</div><div><h2>Claude settings</h2>{{template "id-table" .Report.ClaudeSettings}}</div></div></details>{{end}}
<footer>Generated {{.GeneratedAt}} from <code>{{.Report.ReportDirectory}}</code>. Costs are estimates stored when each request completed. This report does not reprice historical events.</footer>
</main></body></html>
{{define "usage-table"}}{{$rows := index . 0}}{{$total := index . 1}}<div class="table-wrap"><table><thead><tr><th>Name</th><th>Requests</th><th>Success</th><th>Reported input</th><th>Output</th><th>Cache read</th><th>Cache write</th><th>Cost coverage</th><th>Estimated cost</th><th>Known cost share</th><th>Avg / priced request</th></tr></thead><tbody>{{range $rows}}<tr><td class="primary">{{.Name}}</td><td>{{requests .Requests}}</td><td>{{rowSuccess .}}</td><td>{{tokens .KnownInputTokens}}</td><td>{{tokens .KnownOutputTokens}}</td><td>{{tokens .KnownCacheReadInputTokens}}</td><td>{{tokens .KnownCacheWriteInputTokens}}</td><td>{{rowCoverage .}}</td><td>{{rowCost .}}</td><td>{{rowShare . $total}}</td><td>{{rowAverage .}}</td></tr>{{else}}<tr><td colspan="11" class="muted">No generation requests in this period.</td></tr>{{end}}</tbody></table></div>{{end}}
{{define "compact-table"}}<div class="table-wrap"><table><thead><tr><th>Name</th><th>Requests</th><th>Success</th><th>Estimated cost</th><th>Cost coverage</th></tr></thead><tbody>{{range .}}<tr><td class="primary">{{.Name}}</td><td>{{requests .Requests}}</td><td>{{rowSuccess .}}</td><td>{{rowCost .}}</td><td>{{rowCoverage .}}</td></tr>{{else}}<tr><td colspan="5" class="muted">No data.</td></tr>{{end}}</tbody></table></div>{{end}}
{{define "diagnostic-table"}}<div class="table-wrap"><table><thead><tr><th>Name</th><th>Requests</th><th>Failures</th></tr></thead><tbody>{{range .}}<tr><td><code title="{{.Name}}">{{shortID .Name}}</code></td><td>{{requests .Requests}}</td><td>{{requests .Failures}}</td></tr>{{else}}<tr><td colspan="3" class="muted">No data.</td></tr>{{end}}</tbody></table></div>{{end}}
{{define "id-table"}}<div class="table-wrap"><table><thead><tr><th>Identifier</th><th>Requests</th></tr></thead><tbody>{{range .}}<tr><td><code title="{{.Name}}">{{shortID .Name}}</code></td><td>{{requests .Requests}}</td></tr>{{else}}<tr><td colspan="2" class="muted">No data.</td></tr>{{end}}</tbody></table></div>{{end}}`))
