// Package reports aggregates private proxy session artifacts into period reports.
package reports

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/pricing"
)

const schemaVersion = 1

type Options struct {
	Directory   string
	Start, Stop time.Time
	Models      map[string]config.ModelConfig
	Reprice     bool
}
type Report struct {
	GeneratedAt       string             `json:"generated_at"`
	ReportDirectory   string             `json:"report_directory"`
	Start             string             `json:"start"`
	Stop              string             `json:"stop"`
	Sessions          int                `json:"sessions"`
	SkippedSessions   int                `json:"skipped_sessions"`
	Metrics           Metrics            `json:"metrics"`
	Series            []Point            `json:"series"`
	Models            []Breakdown        `json:"models"`
	Endpoints         []Breakdown        `json:"endpoints"`
	SessionTags       []Breakdown        `json:"session_tags"`
	FailureBreakdowns []FailureBreakdown `json:"failure_breakdowns"`
	UsageWarnings     []UsageWarning     `json:"usage_warnings"`
	PricingSource     string             `json:"pricing_source"`
	PricingIssues     []PricingIssue     `json:"pricing_issues"`
	Projection        *Projection        `json:"projection,omitempty"`
}

// Projection is a calendar-month run-rate calculated from the report's
// existing daily trend points. Cost values include only known estimates.
type Projection struct {
	TargetMonth                 string  `json:"target_month"`
	ObservedThrough             string  `json:"observed_through"`
	ElapsedDays                 int     `json:"elapsed_days"`
	DaysInMonth                 int     `json:"days_in_month"`
	KnownEstimatedCost          float64 `json:"known_estimated_cost"`
	Requests                    int     `json:"requests"`
	ProjectedKnownEstimatedCost float64 `json:"projected_known_estimated_cost"`
	ProjectedRequests           int     `json:"projected_requests"`
}

type Metrics struct {
	Requests                   int     `json:"requests"`
	Successes                  int     `json:"successes"`
	Failures                   int     `json:"failures"`
	Canceled                   int     `json:"canceled"`
	KnownInputTokens           int64   `json:"known_input_tokens"`
	KnownOutputTokens          int64   `json:"known_output_tokens"`
	KnownCacheReadInputTokens  int64   `json:"known_cache_read_input_tokens"`
	KnownCacheWriteInputTokens int64   `json:"known_cache_write_input_tokens"`
	KnownEstimatedCost         float64 `json:"known_estimated_cost"`
	MissingUsageRequests       int     `json:"missing_usage_requests"`
	MissingCostRequests        int     `json:"missing_cost_requests"`
	WarnedRequests             int     `json:"warned_requests"`
	RepricedCostRequests       int     `json:"repriced_cost_requests"`
	StoredCostRequests         int     `json:"stored_cost_requests"`
}
type Point struct {
	Start               string  `json:"start"`
	Requests            int     `json:"requests"`
	Successes           int     `json:"successes"`
	Failures            int     `json:"failures"`
	Canceled            int     `json:"canceled"`
	KnownEstimatedCost  float64 `json:"known_estimated_cost"`
	MissingCostRequests int     `json:"missing_cost_requests"`
}
type Breakdown struct {
	Name                string   `json:"name"`
	CurrentAlias        string   `json:"current_alias,omitempty"`
	Aliases             []string `json:"aliases,omitempty"`
	Requests            int      `json:"requests"`
	Successes           int      `json:"successes"`
	Failures            int      `json:"failures"`
	Canceled            int      `json:"canceled"`
	KnownInputTokens    int64    `json:"known_input_tokens"`
	KnownOutputTokens   int64    `json:"known_output_tokens"`
	KnownEstimatedCost  float64  `json:"known_estimated_cost"`
	MissingCostRequests int      `json:"missing_cost_requests"`
}
type FailureBreakdown struct {
	Category   string `json:"category"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Endpoint   string `json:"endpoint"`
	Requests   int    `json:"requests"`
}
type UsageWarning struct {
	Path     string `json:"path"`
	Requests int    `json:"requests"`
}
type PricingIssue struct {
	Model    string `json:"model"`
	Reason   string `json:"reason"`
	Requests int    `json:"requests"`
}
type manifest struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	ReportPath    string `json:"report_path"`
}
type event struct {
	Event                          string   `json:"event"`
	Timestamp                      string   `json:"timestamp"`
	SessionTag                     string   `json:"session_tag"`
	Endpoint                       string   `json:"endpoint"`
	LocalModel                     string   `json:"local_model"`
	UpstreamModel                  string   `json:"upstream_model"`
	Outcome                        string   `json:"outcome"`
	FailureCategory                string   `json:"failure_category"`
	HTTPStatus                     *int     `json:"http_status"`
	InputTokens                    *int64   `json:"input_tokens"`
	OutputTokens                   *int64   `json:"output_tokens"`
	CacheReadInputTokens           *int64   `json:"cache_read_input_tokens"`
	CacheWriteInputTokens          *int64   `json:"cache_write_input_tokens"`
	InputTokensIncludeCache        bool     `json:"input_tokens_include_cache"`
	EstimatedCost                  *float64 `json:"estimated_cost"`
	UsageStatus                    string   `json:"usage_status"`
	CostStatus                     string   `json:"cost_status"`
	UsageWarnings                  []string `json:"usage_warnings"`
	ObservedUncoveredBillingFields bool     `json:"observed_uncovered_billing_fields"`
	ClientFamily                   string   `json:"client_family"`
	ClientVersion                  string   `json:"client_version"`
	MetadataProfile                string   `json:"metadata_profile"`
	MetadataRevision               string   `json:"metadata_revision"`
	CatalogHash                    string   `json:"catalog_hash"`
	SettingsHash                   string   `json:"settings_hash"`
	ContextUtilization             *float64 `json:"context_utilization"`
	OutputUtilization              *float64 `json:"output_utilization"`
	FunctionToolCalls              int      `json:"function_tool_calls"`
	UnsupportedFeatureRejections   int      `json:"unsupported_feature_rejections"`
	costSource                     string
}

func Generate(o Options) (Report, error) {
	if o.Directory == "" {
		return Report{}, fmt.Errorf("report directory is required")
	}
	if !o.Stop.After(o.Start) {
		return Report{}, fmt.Errorf("stop time must be after start time")
	}
	dir, err := filepath.Abs(o.Directory)
	if err != nil {
		return Report{}, err
	}
	source := "recorded request estimates"
	if o.Reprice {
		source = "current configuration (strict repricing)"
	}
	r := Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), ReportDirectory: dir, Start: o.Start.UTC().Format(time.RFC3339Nano), Stop: o.Stop.UTC().Format(time.RFC3339Nano), PricingSource: source}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return Report{}, err
	}
	models := map[string]*Breakdown{}
	endpoints := map[string]*Breakdown{}
	tags := map[string]*Breakdown{}
	points := map[time.Time]*Point{}
	failures := map[string]*FailureBreakdown{}
	warnings := map[string]int{}
	issues := map[string]*PricingIssue{}
	sessions := map[string]bool{}
	bucketDuration := 24 * time.Hour
	if o.Stop.Sub(o.Start) <= 48*time.Hour {
		bucketDuration = time.Hour
	}
	for _, d := range entries {
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		ok, events, e := readSession(filepath.Join(dir, d.Name()))
		if e != nil || !ok {
			r.SkippedSessions++
			continue
		}
		for _, x := range events {
			if x.Event != "request" {
				continue
			}
			at, e := time.Parse(time.RFC3339Nano, x.Timestamp)
			if e != nil || at.Before(o.Start) || !at.Before(o.Stop) {
				continue
			}
			if strings.HasPrefix(x.Endpoint, "/v1/models") {
				continue
			}
			sessions[d.Name()] = true
			if reason := applyPricing(&x, o); reason != "" {
				key := modelIdentity(x) + "\x00" + reason
				if issues[key] == nil {
					issues[key] = &PricingIssue{Model: modelIdentity(x), Reason: reason}
				}
				issues[key].Requests++
			}
			applyMetrics(&r.Metrics, x)
			b := at.UTC().Truncate(bucketDuration)
			if points[b] == nil {
				points[b] = &Point{Start: b.Format(time.RFC3339)}
			}
			applyPoint(points[b], x)
			applyBreakdown(models, modelIdentity(x), x)
			if x.LocalModel != "" {
				addAlias(models[modelIdentity(x)], x.LocalModel)
			}
			applyBreakdown(endpoints, label(x.Endpoint, "unknown endpoint"), x)
			applyBreakdown(tags, label(x.SessionTag, "untagged"), x)
			if x.Outcome != "success" {
				cat := x.FailureCategory
				if cat == "" {
					cat = legacyFailureCategory(x)
				}
				status := 0
				if x.HTTPStatus != nil {
					status = *x.HTTPStatus
				}
				key := cat + "\x00" + fmt.Sprint(status) + "\x00" + x.Endpoint
				if failures[key] == nil {
					failures[key] = &FailureBreakdown{Category: cat, HTTPStatus: status, Endpoint: label(x.Endpoint, "unknown endpoint")}
				}
				failures[key].Requests++
			}
			paths := append([]string(nil), x.UsageWarnings...)
			if x.ObservedUncoveredBillingFields {
				paths = append(paths, "legacy_unidentified_usage_detail")
			}
			if len(paths) > 0 {
				for _, p := range unique(paths) {
					warnings[p]++
				}
			}
		}
	}
	r.Sessions = len(sessions)
	r.Series = orderedPoints(points)
	r.Projection = projectMonthEnd(r.Series)
	addCurrentAliases(models, o.Models)
	r.Models = orderedBreakdowns(models)
	r.Endpoints = orderedBreakdowns(endpoints)
	r.SessionTags = orderedBreakdowns(tags)
	r.FailureBreakdowns = orderedFailures(failures)
	r.UsageWarnings = orderedWarnings(warnings)
	r.PricingIssues = orderedIssues(issues)
	return r, nil
}
func projectMonthEnd(series []Point) *Projection {
	if len(series) == 0 {
		return nil
	}
	latest, err := time.Parse(time.RFC3339, series[len(series)-1].Start)
	if err != nil {
		return nil
	}
	latest = latest.UTC()
	monthStart := time.Date(latest.Year(), latest.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	result := &Projection{TargetMonth: monthStart.Format("2006-01"), ObservedThrough: latest.Format("2006-01-02"), ElapsedDays: latest.Day(), DaysInMonth: int(nextMonth.Sub(monthStart).Hours() / 24)}
	for _, point := range series {
		at, err := time.Parse(time.RFC3339, point.Start)
		if err != nil || at.UTC().Year() != latest.Year() || at.UTC().Month() != latest.Month() {
			continue
		}
		result.KnownEstimatedCost += point.KnownEstimatedCost
		result.Requests += point.Requests
	}
	if result.ElapsedDays == 0 {
		return nil
	}
	result.ProjectedKnownEstimatedCost = result.KnownEstimatedCost / float64(result.ElapsedDays) * float64(result.DaysInMonth)
	result.ProjectedRequests = int(float64(result.Requests)/float64(result.ElapsedDays)*float64(result.DaysInMonth) + 0.5)
	return result
}

func applyPricing(x *event, o Options) string {
	if !o.Reprice {
		if x.EstimatedCost != nil {
			x.costSource = "stored"
		}
		return ""
	}
	x.EstimatedCost = nil
	x.CostStatus = "unavailable"
	model, reason := modelForReprice(*x, o.Models)
	if reason != "" {
		return reason
	}
	cost, pricingReason := pricing.Estimate(model, pricing.Usage{InputTokens: x.InputTokens, OutputTokens: x.OutputTokens, CacheReadInputTokens: x.CacheReadInputTokens, CacheWriteInputTokens: x.CacheWriteInputTokens, InputTokensIncludeCache: repriceInputIncludesCache(*x)})
	if pricingReason != "" {
		return string(pricingReason)
	}
	x.EstimatedCost = &cost
	x.CostStatus = "estimated"
	x.costSource = "repriced"
	return ""
}
func repriceInputIncludesCache(x event) bool {
	return x.InputTokensIncludeCache || x.Endpoint == "/v1/responses" || x.Endpoint == "/v1/chat/completions"
}

func addCurrentAliases(rows map[string]*Breakdown, configured map[string]config.ModelConfig) {
	aliases := make([]string, 0, len(configured))
	for alias := range configured {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		model := configured[alias]
		if row := rows[model.BedrockModelID]; row != nil && row.CurrentAlias == "" {
			row.CurrentAlias = alias
		}
	}
}

func modelForReprice(x event, models map[string]config.ModelConfig) (config.ModelConfig, string) {
	target := x.UpstreamModel
	if target == "" {
		m, ok := models[x.LocalModel]
		if !ok {
			return config.ModelConfig{}, "model alias is absent from the current configuration"
		}
		return m, ""
	}
	var found []config.ModelConfig
	for _, m := range models {
		if m.BedrockModelID == target {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		return config.ModelConfig{}, "upstream model is absent from the current configuration"
	}
	base := found[0]
	for _, m := range found[1:] {
		if !pricing.EquivalentRates(base, m) {
			return config.ModelConfig{}, "conflicting pricing for upstream model"
		}
	}
	return base, ""
}
func modelIdentity(x event) string {
	if x.UpstreamModel != "" {
		return x.UpstreamModel
	}
	return label(x.LocalModel, "unresolved model")
}
func legacyFailureCategory(x event) string {
	if x.Outcome == "canceled" {
		return "canceled"
	}
	if x.HTTPStatus != nil && *x.HTTPStatus >= 400 && *x.HTTPStatus < 500 {
		return "local_validation"
	}
	return "internal"
}
func applyMetrics(m *Metrics, x event) {
	m.Requests++
	if x.Outcome == "success" {
		m.Successes++
	} else if x.Outcome == "canceled" {
		m.Canceled++
	} else {
		m.Failures++
	}
	if x.InputTokens != nil {
		m.KnownInputTokens += *x.InputTokens
	}
	if x.OutputTokens != nil {
		m.KnownOutputTokens += *x.OutputTokens
	}
	if x.CacheReadInputTokens != nil {
		m.KnownCacheReadInputTokens += *x.CacheReadInputTokens
	}
	if x.CacheWriteInputTokens != nil {
		m.KnownCacheWriteInputTokens += *x.CacheWriteInputTokens
	}
	if x.InputTokens == nil || x.OutputTokens == nil {
		m.MissingUsageRequests++
	}
	if x.EstimatedCost == nil {
		m.MissingCostRequests++
	} else {
		m.KnownEstimatedCost += *x.EstimatedCost
		if x.costSource == "stored" {
			m.StoredCostRequests++
		}
		if x.costSource == "repriced" {
			m.RepricedCostRequests++
		}
	}
	if len(x.UsageWarnings) > 0 || x.ObservedUncoveredBillingFields {
		m.WarnedRequests++
	}
}
func applyPoint(p *Point, x event) {
	p.Requests++
	if x.Outcome == "success" {
		p.Successes++
	} else if x.Outcome == "canceled" {
		p.Canceled++
	} else {
		p.Failures++
	}
	if x.EstimatedCost == nil {
		p.MissingCostRequests++
	} else {
		p.KnownEstimatedCost += *x.EstimatedCost
	}
}
func applyBreakdown(all map[string]*Breakdown, name string, x event) {
	b := all[name]
	if b == nil {
		b = &Breakdown{Name: name}
		all[name] = b
	}
	b.Requests++
	if x.Outcome == "success" {
		b.Successes++
	} else if x.Outcome == "canceled" {
		b.Canceled++
	} else {
		b.Failures++
	}
	if x.InputTokens != nil {
		b.KnownInputTokens += *x.InputTokens
	}
	if x.OutputTokens != nil {
		b.KnownOutputTokens += *x.OutputTokens
	}
	if x.EstimatedCost == nil {
		b.MissingCostRequests++
	} else {
		b.KnownEstimatedCost += *x.EstimatedCost
	}
}
func addAlias(b *Breakdown, a string) {
	for _, v := range b.Aliases {
		if v == a {
			return
		}
	}
	b.Aliases = append(b.Aliases, a)
	sort.Strings(b.Aliases)
}
func orderedPoints(m map[time.Time]*Point) []Point {
	keys := make([]time.Time, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	r := make([]Point, 0, len(keys))
	for _, k := range keys {
		r = append(r, *m[k])
	}
	return r
}
func orderedBreakdowns(m map[string]*Breakdown) []Breakdown {
	r := make([]Breakdown, 0, len(m))
	for _, v := range m {
		r = append(r, *v)
	}
	sort.Slice(r, func(i, j int) bool {
		if r[i].KnownEstimatedCost != r[j].KnownEstimatedCost {
			return r[i].KnownEstimatedCost > r[j].KnownEstimatedCost
		}
		return r[i].Name < r[j].Name
	})
	return r
}
func orderedFailures(m map[string]*FailureBreakdown) []FailureBreakdown {
	r := make([]FailureBreakdown, 0, len(m))
	for _, v := range m {
		r = append(r, *v)
	}
	sort.Slice(r, func(i, j int) bool {
		if r[i].Requests != r[j].Requests {
			return r[i].Requests > r[j].Requests
		}
		if r[i].Category != r[j].Category {
			return r[i].Category < r[j].Category
		}
		return r[i].Endpoint < r[j].Endpoint
	})
	return r
}
func orderedWarnings(m map[string]int) []UsageWarning {
	r := make([]UsageWarning, 0, len(m))
	for p, n := range m {
		r = append(r, UsageWarning{p, n})
	}
	sort.Slice(r, func(i, j int) bool { return r[i].Path < r[j].Path })
	return r
}
func orderedIssues(m map[string]*PricingIssue) []PricingIssue {
	r := make([]PricingIssue, 0, len(m))
	for _, v := range m {
		r = append(r, *v)
	}
	sort.Slice(r, func(i, j int) bool {
		if r[i].Requests != r[j].Requests {
			return r[i].Requests > r[j].Requests
		}
		if r[i].Model != r[j].Model {
			return r[i].Model < r[j].Model
		}
		return r[i].Reason < r[j].Reason
	})
	return r
}
func unique(v []string) []string {
	sort.Strings(v)
	o := v[:0]
	for _, x := range v {
		if x != "" && (len(o) == 0 || o[len(o)-1] != x) {
			o = append(o, x)
		}
	}
	return o
}
func label(v, f string) string {
	if strings.TrimSpace(v) == "" {
		return f
	}
	return v
}
func readSession(path string) (bool, []event, error) {
	info, e := os.Lstat(path)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil, e
	}
	data, e := os.ReadFile(filepath.Join(path, "session.json"))
	if e != nil {
		return false, nil, e
	}
	var m manifest
	if e = json.Unmarshal(data, &m); e != nil || m.SchemaVersion != schemaVersion || m.SessionID != filepath.Base(path) || filepath.Clean(m.ReportPath) != filepath.Clean(path) {
		return false, nil, e
	}
	f, e := os.Open(filepath.Join(path, "events.jsonl"))
	if e != nil {
		return false, nil, e
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 65536), 1048576)
	var r []event
	for s.Scan() {
		var x event
		if e = json.Unmarshal(s.Bytes(), &x); e != nil {
			return false, nil, e
		}
		r = append(r, x)
	}
	return s.Err() == nil, r, s.Err()
}
