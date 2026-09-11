// Package reports aggregates private proxy session artifacts into period reports.
package reports

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const schemaVersion = 1

// Options describes an inclusive-start, exclusive-stop report window.
type Options struct {
	Directory string
	Start     time.Time
	Stop      time.Time
}

// Report is the portable, metadata-only summary for a selected period.
type Report struct {
	GeneratedAt     string      `json:"generated_at"`
	ReportDirectory string      `json:"report_directory"`
	Start           string      `json:"start"`
	Stop            string      `json:"stop"`
	Sessions        int         `json:"sessions"`
	SkippedSessions int         `json:"skipped_sessions"`
	Metrics         Metrics     `json:"metrics"`
	Series          []Point     `json:"series"`
	Models          []Breakdown `json:"models"`
	Endpoints       []Breakdown `json:"endpoints"`
	SessionTags     []Breakdown `json:"session_tags"`
}

type Metrics struct {
	Requests             int     `json:"requests"`
	Successes            int     `json:"successes"`
	Failures             int     `json:"failures"`
	Canceled             int     `json:"canceled"`
	ModelListEvents      int     `json:"model_list_events"`
	KnownInputTokens     int64   `json:"known_input_tokens"`
	KnownOutputTokens    int64   `json:"known_output_tokens"`
	KnownEstimatedCost   float64 `json:"known_estimated_cost"`
	MissingUsageRequests int     `json:"missing_usage_requests"`
	MissingCostRequests  int     `json:"missing_cost_requests"`
}

type Point struct {
	Start              string  `json:"start"`
	Requests           int     `json:"requests"`
	Successes          int     `json:"successes"`
	Failures           int     `json:"failures"`
	Canceled           int     `json:"canceled"`
	KnownEstimatedCost float64 `json:"known_estimated_cost"`
}

type Breakdown struct {
	Name               string  `json:"name"`
	Requests           int     `json:"requests"`
	Successes          int     `json:"successes"`
	Failures           int     `json:"failures"`
	Canceled           int     `json:"canceled"`
	KnownInputTokens   int64   `json:"known_input_tokens"`
	KnownOutputTokens  int64   `json:"known_output_tokens"`
	KnownEstimatedCost float64 `json:"known_estimated_cost"`
}

type manifest struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	ReportPath    string `json:"report_path"`
}

type event struct {
	Event         string   `json:"event"`
	Timestamp     string   `json:"timestamp"`
	SessionTag    string   `json:"session_tag"`
	Endpoint      string   `json:"endpoint"`
	LocalModel    string   `json:"local_model"`
	Outcome       string   `json:"outcome"`
	InputTokens   *int64   `json:"input_tokens"`
	OutputTokens  *int64   `json:"output_tokens"`
	EstimatedCost *float64 `json:"estimated_cost"`
	UsageStatus   string   `json:"usage_status"`
	CostStatus    string   `json:"cost_status"`
}

// Generate reads recognized report directories and aggregates request events
// inside the requested period. Malformed or incomplete artifacts are skipped.
func Generate(options Options) (Report, error) {
	if options.Directory == "" {
		return Report{}, fmt.Errorf("report directory is required")
	}
	if !options.Stop.After(options.Start) {
		return Report{}, fmt.Errorf("stop time must be after start time")
	}
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return Report{}, fmt.Errorf("resolve report directory: %w", err)
	}
	report := Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), ReportDirectory: directory, Start: options.Start.UTC().Format(time.RFC3339Nano), Stop: options.Stop.UTC().Format(time.RFC3339Nano)}
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return Report{}, fmt.Errorf("read report directory: %w", err)
	}
	byBucket := map[time.Time]*Point{}
	byModel := map[string]*Breakdown{}
	byEndpoint := map[string]*Breakdown{}
	byTag := map[string]*Breakdown{}
	contributingSessions := map[string]struct{}{}
	bucketDuration := 24 * time.Hour
	if options.Stop.Sub(options.Start) <= 48*time.Hour {
		bucketDuration = time.Hour
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		sessionPath := filepath.Join(directory, entry.Name())
		if _, err := os.Lstat(filepath.Join(sessionPath, "session.json")); err != nil {
			if !os.IsNotExist(err) {
				report.SkippedSessions++
			}
			continue
		}
		valid, events, err := readSession(sessionPath)
		if err != nil || !valid {
			report.SkippedSessions++
			continue
		}
		for _, item := range events {
			if item.Event != "request" {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, item.Timestamp)
			if err != nil || at.Before(options.Start) || !at.Before(options.Stop) {
				continue
			}
			contributingSessions[entry.Name()] = struct{}{}
			if item.Endpoint == "/v1/models" {
				report.Metrics.ModelListEvents++
				continue
			}
			applyMetrics(&report.Metrics, item)
			bucket := at.UTC().Truncate(bucketDuration)
			point := byBucket[bucket]
			if point == nil {
				point = &Point{Start: bucket.Format(time.RFC3339)}
				byBucket[bucket] = point
			}
			applyPoint(point, item)
			applyBreakdown(byModel, label(item.LocalModel, "unresolved model"), item)
			applyBreakdown(byEndpoint, label(item.Endpoint, "unknown endpoint"), item)
			applyBreakdown(byTag, label(item.SessionTag, "untagged"), item)
		}
	}
	report.Sessions = len(contributingSessions)
	report.Series = orderedPoints(byBucket)
	report.Models = orderedBreakdowns(byModel)
	report.Endpoints = orderedBreakdowns(byEndpoint)
	report.SessionTags = orderedBreakdowns(byTag)
	return report, nil
}

func readSession(path string) (bool, []event, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil, err
	}
	manifestPath := filepath.Join(path, "session.json")
	info, err = os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, nil, err
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil || value.SchemaVersion != schemaVersion || value.SessionID != filepath.Base(path) || filepath.Clean(value.ReportPath) != filepath.Clean(path) {
		return false, nil, err
	}
	eventsPath := filepath.Join(path, "events.jsonl")
	info, err = os.Lstat(eventsPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil, err
	}
	file, err := os.Open(eventsPath)
	if err != nil {
		return false, nil, err
	}
	defer file.Close()
	var result []event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var item event
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return false, nil, err
		}
		result = append(result, item)
	}
	if err := scanner.Err(); err != nil {
		return false, nil, err
	}
	return true, result, nil
}

func applyMetrics(metrics *Metrics, item event) {
	metrics.Requests++
	switch item.Outcome {
	case "success":
		metrics.Successes++
	case "canceled":
		metrics.Canceled++
		metrics.Failures++
	default:
		metrics.Failures++
	}
	if item.InputTokens != nil {
		metrics.KnownInputTokens += *item.InputTokens
	}
	if item.OutputTokens != nil {
		metrics.KnownOutputTokens += *item.OutputTokens
	}
	if item.UsageStatus != "known" {
		metrics.MissingUsageRequests++
	}
	if item.EstimatedCost != nil {
		metrics.KnownEstimatedCost += *item.EstimatedCost
	} else {
		metrics.MissingCostRequests++
	}
}

func applyPoint(point *Point, item event) {
	point.Requests++
	switch item.Outcome {
	case "success":
		point.Successes++
	case "canceled":
		point.Canceled++
		point.Failures++
	default:
		point.Failures++
	}
	if item.EstimatedCost != nil {
		point.KnownEstimatedCost += *item.EstimatedCost
	}
}

func applyBreakdown(values map[string]*Breakdown, name string, item event) {
	value := values[name]
	if value == nil {
		value = &Breakdown{Name: name}
		values[name] = value
	}
	value.Requests++
	switch item.Outcome {
	case "success":
		value.Successes++
	case "canceled":
		value.Canceled++
		value.Failures++
	default:
		value.Failures++
	}
	if item.InputTokens != nil {
		value.KnownInputTokens += *item.InputTokens
	}
	if item.OutputTokens != nil {
		value.KnownOutputTokens += *item.OutputTokens
	}
	if item.EstimatedCost != nil {
		value.KnownEstimatedCost += *item.EstimatedCost
	}
}

func orderedPoints(values map[time.Time]*Point) []Point {
	keys := make([]time.Time, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	result := make([]Point, 0, len(keys))
	for _, key := range keys {
		result = append(result, *values[key])
	}
	return result
}

func orderedBreakdowns(values map[string]*Breakdown) []Breakdown {
	result := make([]Breakdown, 0, len(values))
	for _, value := range values {
		result = append(result, *value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].KnownEstimatedCost != result[j].KnownEstimatedCost {
			return result[i].KnownEstimatedCost > result[j].KnownEstimatedCost
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func label(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

type htmlView struct {
	Report      Report
	SuccessRate string
	RequestBars []bar
	CostBars    []bar
}

type bar struct {
	Label  string
	Value  string
	Height int
}

// HTML renders a standalone report with inline CSS and SVG charts.
func HTML(report Report) ([]byte, error) {
	view := htmlView{Report: report, RequestBars: makeBars(report.Series, func(point Point) float64 { return float64(point.Requests) }, func(point Point) string { return fmt.Sprintf("%d requests", point.Requests) }), CostBars: makeBars(report.Series, func(point Point) float64 { return point.KnownEstimatedCost }, func(point Point) string { return fmt.Sprintf("$%.4f", point.KnownEstimatedCost) })}
	if report.Metrics.Requests > 0 {
		view.SuccessRate = fmt.Sprintf("%.1f%%", float64(report.Metrics.Successes)*100/float64(report.Metrics.Requests))
	} else {
		view.SuccessRate = "n/a"
	}
	var output bytes.Buffer
	if err := htmlTemplate.Execute(&output, view); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func makeBars(points []Point, number func(Point) float64, text func(Point) string) []bar {
	max := 0.0
	for _, point := range points {
		max = math.Max(max, number(point))
	}
	result := make([]bar, 0, len(points))
	for _, point := range points {
		height := 0
		if max > 0 {
			height = int(math.Round(number(point) / max * 160))
		}
		result = append(result, bar{Label: point.Start, Value: text(point), Height: height})
	}
	return result
}

var htmlTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Bedrock Local Proxy report</title><style>
body{font:15px system-ui,sans-serif;max-width:1120px;margin:32px auto;padding:0 20px;color:#18212f;background:#f7f8fa}h1,h2{color:#101827}.sub{color:#526070}.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px}.card,section{background:white;border:1px solid #dce1e8;border-radius:10px;padding:16px;margin:16px 0}.card b{font-size:24px;display:block}.charts{display:grid;grid-template-columns:1fr 1fr;gap:16px}.chart{height:220px;display:flex;align-items:end;gap:3px;border-bottom:1px solid #bac4d1;padding:0 4px}.bar{min-width:8px;flex:1;background:#3973b9;border-radius:3px 3px 0 0}.cost .bar{background:#17875b}table{border-collapse:collapse;width:100%;margin-top:8px}th,td{padding:8px;text-align:left;border-bottom:1px solid #e5e8ed}th{color:#526070;font-weight:600}@media(max-width:700px){.charts{grid-template-columns:1fr}}</style></head><body>
<h1>Bedrock Local Proxy usage report</h1><p class="sub">{{.Report.Start}} to {{.Report.Stop}} UTC · generated {{.Report.GeneratedAt}}</p>
<div class="cards"><div class="card"><span>Requests</span><b>{{.Report.Metrics.Requests}}</b></div><div class="card"><span>Success rate</span><b>{{.SuccessRate}}</b></div><div class="card"><span>Estimated cost</span><b>${{printf "%.4f" .Report.Metrics.KnownEstimatedCost}}</b></div><div class="card"><span>Tokens</span><b>{{.Report.Metrics.KnownInputTokens}} / {{.Report.Metrics.KnownOutputTokens}}</b><span>input / output</span></div><div class="card"><span>Sessions</span><b>{{.Report.Sessions}}</b></div></div>
<section><h2>Coverage</h2><p>{{.Report.Metrics.Successes}} successful · {{.Report.Metrics.Failures}} failed ({{.Report.Metrics.Canceled}} canceled) · {{.Report.Metrics.MissingUsageRequests}} requests without complete usage · {{.Report.Metrics.MissingCostRequests}} requests without an estimate · {{.Report.Metrics.ModelListEvents}} model-list events excluded from totals · {{.Report.SkippedSessions}} unreadable or unrecognized session directories skipped.</p></section>
<div class="charts"><section><h2>Requests over time</h2><div class="chart">{{range .RequestBars}}<div class="bar" style="height:{{.Height}}px" title="{{.Label}}: {{.Value}}"></div>{{end}}</div></section><section><h2>Estimated cost over time</h2><div class="chart cost">{{range .CostBars}}<div class="bar" style="height:{{.Height}}px" title="{{.Label}}: {{.Value}}"></div>{{end}}</div></section></div>
{{template "breakdown" .Report.Models}}<section><h2>Endpoint breakdown</h2>{{template "rows" .Report.Endpoints}}</section><section><h2>Session tag breakdown</h2>{{template "rows" .Report.SessionTags}}</section>
</body></html>{{define "breakdown"}}<section><h2>Model breakdown</h2>{{template "rows" .}}</section>{{end}}{{define "rows"}}<table><thead><tr><th>Name</th><th>Requests</th><th>Successes</th><th>Failures</th><th>Input tokens</th><th>Output tokens</th><th>Estimated cost</th></tr></thead><tbody>{{range .}}<tr><td>{{.Name}}</td><td>{{.Requests}}</td><td>{{.Successes}}</td><td>{{.Failures}}</td><td>{{.KnownInputTokens}}</td><td>{{.KnownOutputTokens}}</td><td>${{printf "%.6f" .KnownEstimatedCost}}</td></tr>{{else}}<tr><td colspan="7">No generation requests in this period.</td></tr>{{end}}</tbody></table>{{end}}`))
