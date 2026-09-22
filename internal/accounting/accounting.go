// Package accounting records metadata-only request outcomes and session totals.
package accounting

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/console"
	"github.com/gregasher/bedrock-local-proxy/internal/pricing"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

type totals struct {
	requests, successes, failures               int
	knownInputTokens, knownOutputTokens         int64
	knownCacheReadTokens, knownCacheWriteTokens int64
	knownEstimatedCost                          float64
	missingUsageRequests, missingEstimates      int
	incompleteRequests                          int
	functionToolCalls, unsupportedRejections    int
	uncountedSuccesses                          int
}

type Recorder struct {
	mu      sync.Mutex
	w       io.Writer
	format  Format
	models  map[string]config.ModelConfig
	started time.Time
	totals  totals
	session *SessionReporter
	out     *console.Console
}

type requestRecord struct {
	Event                          string   `json:"event"`
	Timestamp                      string   `json:"timestamp"`
	SessionID                      string   `json:"session_id,omitempty"`
	SessionTag                     string   `json:"session_tag,omitempty"`
	Preset                         string   `json:"preset,omitempty"`
	Endpoint                       string   `json:"endpoint"`
	LocalModel                     string   `json:"local_model,omitempty"`
	UpstreamModel                  string   `json:"upstream_model,omitempty"`
	LatencyMS                      float64  `json:"latency_ms"`
	HTTPStatus                     *int     `json:"http_status,omitempty"`
	Outcome                        string   `json:"outcome"`
	FailureCategory                string   `json:"failure_category,omitempty"`
	UsageWarnings                  []string `json:"usage_warnings,omitempty"`
	InputTokens                    *int64   `json:"input_tokens,omitempty"`
	OutputTokens                   *int64   `json:"output_tokens,omitempty"`
	CacheReadInputTokens           *int64   `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens          *int64   `json:"cache_write_input_tokens,omitempty"`
	InputTokensIncludeCache        bool     `json:"input_tokens_include_cache,omitempty"`
	EstimatedCost                  *float64 `json:"estimated_cost,omitempty"`
	CostStatus                     string   `json:"cost_status"`
	UsageStatus                    string   `json:"usage_status"`
	ObservedUncoveredBillingFields bool     `json:"observed_uncovered_billing_fields,omitempty"`
	ClientFamily                   string   `json:"client_family,omitempty"`
	ClientVersion                  string   `json:"client_version,omitempty"`
	MetadataProfile                string   `json:"metadata_profile,omitempty"`
	MetadataRevision               string   `json:"metadata_revision,omitempty"`
	CatalogHash                    string   `json:"catalog_hash,omitempty"`
	SettingsHash                   string   `json:"settings_hash,omitempty"`
	ContextUtilization             *float64 `json:"context_utilization,omitempty"`
	OutputUtilization              *float64 `json:"output_utilization,omitempty"`
	FunctionToolCalls              int      `json:"function_tool_calls,omitempty"`
	UnsupportedFeatureRejections   int      `json:"unsupported_feature_rejections,omitempty"`
}

type summaryRecord struct {
	Event                        string  `json:"event"`
	SessionID                    string  `json:"session_id,omitempty"`
	SessionTag                   string  `json:"session_tag,omitempty"`
	Preset                       string  `json:"preset,omitempty"`
	StartedAt                    string  `json:"started_at,omitempty"`
	FinishedAt                   string  `json:"finished_at,omitempty"`
	Status                       string  `json:"status,omitempty"`
	RuntimeSeconds               float64 `json:"runtime_seconds"`
	Requests                     int     `json:"requests"`
	Successes                    int     `json:"successes"`
	Failures                     int     `json:"failures"`
	KnownInputTokens             int64   `json:"known_input_tokens"`
	KnownOutputTokens            int64   `json:"known_output_tokens"`
	KnownCacheReadInputTokens    int64   `json:"known_cache_read_input_tokens"`
	KnownCacheWriteInputTokens   int64   `json:"known_cache_write_input_tokens"`
	KnownEstimatedCost           float64 `json:"known_estimated_cost"`
	MissingUsageRequests         int     `json:"missing_usage_requests"`
	MissingEstimateRequests      int     `json:"missing_estimate_requests"`
	IncompleteRequests           int     `json:"incomplete_requests"`
	CostStatus                   string  `json:"cost_status"`
	FunctionToolCalls            int     `json:"function_tool_calls"`
	UnsupportedFeatureRejections int     `json:"unsupported_feature_rejections"`
}

func New(w io.Writer, format Format, models map[string]config.ModelConfig) *Recorder {
	copyModels := make(map[string]config.ModelConfig, len(models))
	for name, model := range models {
		copyModels[name] = model
	}
	recorder := &Recorder{w: w, format: format, models: copyModels, started: time.Now()}
	if format == FormatText {
		recorder.out = console.New(w, console.ColorAuto)
	}
	return recorder
}

// SetConsole shares the command's console so request lines print above the
// live footer instead of overwriting it.
func (r *Recorder) SetConsole(out *console.Console) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = out
}

// Status reports live totals for the console footer.
func (r *Recorder) Status() console.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return console.Status{Requests: r.totals.requests, Failures: r.totals.failures, Spent: r.totals.knownEstimatedCost, Uptime: time.Since(r.started)}
}

// SetSessionReporter attaches the durable, metadata-only sink used for one
// proxy run. It should be called before the server accepts requests.
func (r *Recorder) SetSessionReporter(reporter *SessionReporter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session = reporter
}

func (r *Recorder) Record(result server.CompletionResult) {
	observed := time.Now()
	input, output := result.InputTokens, result.OutputTokens
	cost, priceReason := r.estimate(result)
	costOK := priceReason == ""
	usageStatus := "known"
	if input == nil || output == nil {
		usageStatus = "unknown"
	}
	entry := requestRecord{Event: "request", Timestamp: observed.UTC().Format(time.RFC3339Nano), Endpoint: result.Endpoint,
		LocalModel: result.LocalModel, UpstreamModel: result.UpstreamModel, LatencyMS: float64(result.Elapsed) / float64(time.Millisecond),
		HTTPStatus: result.HTTPStatus, Outcome: string(result.Outcome), InputTokens: input, OutputTokens: output,
		CacheReadInputTokens: result.CacheReadInputTokens, CacheWriteInputTokens: result.CacheWriteInputTokens, InputTokensIncludeCache: result.InputTokensIncludeCache,
		CostStatus: "unavailable", UsageStatus: usageStatus, ObservedUncoveredBillingFields: result.ObservedUncoveredBillingFields, UsageWarnings: sortedWarnings(result.UsageWarnings), FailureCategory: result.FailureCategory,
		ClientFamily: result.ClientFamily, ClientVersion: result.ClientVersion, MetadataProfile: result.MetadataProfile,
		MetadataRevision: result.MetadataRevision, CatalogHash: result.CatalogHash, SettingsHash: result.SettingsHash, FunctionToolCalls: result.FunctionToolCalls,
		UnsupportedFeatureRejections: result.UnsupportedFeatureRejections}
	if input != nil && output != nil && result.ContextWindow > 0 {
		contextInput := *input
		if !result.InputTokensIncludeCache {
			contextInput += tokenCount(result.CacheReadInputTokens) + tokenCount(result.CacheWriteInputTokens)
		}
		value := float64(contextInput+*output) / float64(result.ContextWindow)
		entry.ContextUtilization = &value
	}
	if output != nil && result.MaxOutputTokens > 0 {
		value := float64(*output) / float64(result.MaxOutputTokens)
		entry.OutputUtilization = &value
	}
	if costOK {
		entry.EstimatedCost = &cost
		entry.CostStatus = "estimated"
	}
	if entry.Outcome != string(server.CompletionSucceeded) && entry.FailureCategory == "" {
		entry.FailureCategory = defaultFailureCategory(result)
	}
	r.mu.Lock()
	if !strings.HasPrefix(result.Endpoint, "/v1/models") {
		r.totals.requests++
		if result.Outcome == server.CompletionSucceeded {
			r.totals.successes++
		} else {
			r.totals.failures++
		}
		if input != nil {
			r.totals.knownInputTokens += *input
		}
		if output != nil {
			r.totals.knownOutputTokens += *output
		}
		if result.CacheReadInputTokens != nil {
			r.totals.knownCacheReadTokens += *result.CacheReadInputTokens
		}
		if result.CacheWriteInputTokens != nil {
			r.totals.knownCacheWriteTokens += *result.CacheWriteInputTokens
		}
		if usageStatus != "known" {
			r.totals.missingUsageRequests++
		}
		if !costOK {
			r.totals.missingEstimates++
			// A succeeded request that cannot be priced for a configuration
			// reason is real spend this session will never account for.
			// Absent usage is excluded: it is a property of the client's
			// request, not of a fixable price, and a budget cannot demand it.
			if result.Outcome == server.CompletionSucceeded && priceReason != pricing.MissingUsage {
				r.totals.uncountedSuccesses++
			}
		} else {
			r.totals.knownEstimatedCost += cost
		}
		r.totals.functionToolCalls += result.FunctionToolCalls
		r.totals.unsupportedRejections += result.UnsupportedFeatureRejections
	}
	// Every other sink is released from this lock on purpose. The console
	// footer reads these same totals back through Status, so holding the
	// recorder lock across a console write would deadlock the proxy.
	session, out := r.session, r.out
	r.mu.Unlock()
	if session != nil {
		session.RecordRequest(entry)
	}
	if r.w != nil {
		if r.format == FormatJSON {
			_ = json.NewEncoder(r.w).Encode(entry)
			return
		}
		if out != nil {
			out.Print(requestLine(out, entry, observed))
		}
	}
}

func (r *Recorder) estimate(result server.CompletionResult) (float64, pricing.Reason) {
	model, ok := r.models[result.LocalModel]
	if !ok {
		return 0, pricing.UnknownModel
	}
	return pricing.Estimate(model, pricing.Usage{
		InputTokens:                    result.InputTokens,
		OutputTokens:                   result.OutputTokens,
		CacheReadInputTokens:           result.CacheReadInputTokens,
		CacheWriteInputTokens:          result.CacheWriteInputTokens,
		InputTokensIncludeCache:        result.InputTokensIncludeCache,
		ObservedUncoveredBillingFields: result.ObservedUncoveredBillingFields,
	})
}

func sortedWarnings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	out := copyValues[:0]
	for _, value := range copyValues {
		if value != "" && (len(out) == 0 || out[len(out)-1] != value) {
			out = append(out, value)
		}
	}
	return out
}

func defaultFailureCategory(result server.CompletionResult) string {
	if result.Outcome == server.CompletionCanceled {
		return "canceled"
	}
	if result.HTTPStatus != nil && *result.HTTPStatus >= 400 && *result.HTTPStatus < 500 {
		return "local_validation"
	}
	return "internal"
}

func tokenCount(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (r *Recorder) WriteSummary() {
	r.mu.Lock()
	s := summaryRecord{Event: "summary", RuntimeSeconds: time.Since(r.started).Seconds(), Requests: r.totals.requests, Successes: r.totals.successes, Failures: r.totals.failures,
		KnownInputTokens: r.totals.knownInputTokens, KnownOutputTokens: r.totals.knownOutputTokens, KnownEstimatedCost: r.totals.knownEstimatedCost,
		KnownCacheReadInputTokens: r.totals.knownCacheReadTokens, KnownCacheWriteInputTokens: r.totals.knownCacheWriteTokens,
		MissingUsageRequests: r.totals.missingUsageRequests, MissingEstimateRequests: r.totals.missingEstimates, IncompleteRequests: r.totals.incompleteRequests, CostStatus: "unavailable"}
	s.FunctionToolCalls = r.totals.functionToolCalls
	s.UnsupportedFeatureRejections = r.totals.unsupportedRejections
	if r.totals.requests > 0 && r.totals.missingEstimates == 0 {
		s.CostStatus = "estimated"
	} else if r.totals.requests > r.totals.missingEstimates {
		s.CostStatus = "partial"
	}
	session, out := r.session, r.out
	r.mu.Unlock()
	if session != nil {
		session.StageSummary(s)
	}
	if r.w != nil {
		if r.format == FormatJSON {
			_ = json.NewEncoder(r.w).Encode(s)
			return
		}
		if out != nil {
			out.Summary(console.Metrics{
				Runtime: time.Duration(s.RuntimeSeconds * float64(time.Second)), Requests: s.Requests, Successes: s.Successes, Failures: s.Failures,
				InputTokens: s.KnownInputTokens, OutputTokens: s.KnownOutputTokens, CacheReadTokens: s.KnownCacheReadInputTokens, CacheWriteTokens: s.KnownCacheWriteInputTokens,
				EstimatedCost: s.KnownEstimatedCost, CostStatus: s.CostStatus, MissingUsage: s.MissingUsageRequests, MissingEstimates: s.MissingEstimateRequests, Incomplete: s.IncompleteRequests,
			})
		}
	}
}

// BudgetSpend reports what a spend ceiling is enforced against: the
// accumulated estimated cost, and the number of successful requests whose
// cost this session could not account for at all.
func (r *Recorder) BudgetSpend() server.BudgetSpend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return server.BudgetSpend{Estimated: r.totals.knownEstimatedCost, Uncounted: r.totals.uncountedSuccesses}
}

// MarkIncomplete records active handlers that did not publish a completion
// before the bounded shutdown grace expired. They remain outside request
// totals, so requests still equals successes plus failures.
func (r *Recorder) MarkIncomplete(count int) {
	if count < 0 {
		count = 0
	}
	r.mu.Lock()
	r.totals.incompleteRequests += count
	r.mu.Unlock()
}

// requestLine renders one completion as a fixed-column row. Fields that carry
// no information are omitted rather than printed as "unavailable"; the
// complete record is always in the session report and in --log-format json.
func requestLine(out *console.Console, entry requestRecord, observed time.Time) string {
	return requestLineAtWidth(out, entry, observed, out.Width())
}

func requestLineAtWidth(out *console.Console, entry requestRecord, observed time.Time, width int) string {
	succeeded := entry.Outcome == string(server.CompletionSucceeded)
	route := strings.TrimPrefix(entry.Endpoint, "/v1/")
	if index := strings.Index(route, "/"); index >= 0 {
		route = route[:index]
	}
	showTime := width == 0 || width >= 54
	showModel := succeeded || width == 0 || width >= 48
	showUsage := width == 0 || width >= 78
	showCache := width == 0 || width >= 91
	showCost := width == 0 || width >= 48
	if !succeeded {
		showTime = width == 0 || width >= 64
		showUsage = width == 0 || width >= 100
		showCache = width == 0 || width >= 120
		showCost = width == 0 || width >= 76
	}

	columns := []string{
		out.Glyph(succeeded),
		padCell(route, 10),
	}
	if showTime {
		columns = append([]string{out.Style(observed.Format("15:04:05"), console.Dim)}, columns...)
	}
	if showModel {
		columns = append(columns, out.Style(padCell(entry.LocalModel, 12), console.Cyan))
	}
	columns = append(columns, padLeft(console.Duration(time.Duration(entry.LatencyMS*float64(time.Millisecond))), 7))
	if showUsage && (entry.InputTokens != nil || entry.OutputTokens != nil) {
		columns = append(columns, padCell("in "+console.Tokens(deref(entry.InputTokens)), 11), padCell("out "+console.Tokens(deref(entry.OutputTokens)), 11))
	} else if showUsage {
		columns = append(columns, padCell("", 11), padCell("", 11))
	}
	if cache := deref(entry.CacheReadInputTokens) + deref(entry.CacheWriteInputTokens); showCache && cache > 0 {
		columns = append(columns, out.Style(padCell("cache "+console.Tokens(cache), 12), console.Dim))
	}
	if showCost && entry.EstimatedCost != nil {
		columns = append(columns, out.Style(console.Money(*entry.EstimatedCost), console.Yellow))
	}
	if !succeeded {
		detail := entry.FailureCategory
		if entry.HTTPStatus != nil {
			detail = fmt.Sprintf("%d %s", *entry.HTTPStatus, detail)
		}
		if width > 0 {
			used := len([]rune(stripStyles(strings.Join(columns, " ")))) + 1
			detail = truncateCell(strings.TrimSpace(detail), width-used)
		}
		columns = append(columns, out.Style(strings.TrimSpace(detail), console.Red))
	}
	line := strings.TrimRight(strings.Join(columns, " "), " ")
	// A discovery call is excluded from session totals, so it is dimmed to
	// keep generation traffic the thing that stands out.
	if strings.HasPrefix(entry.Endpoint, "/v1/models") {
		return out.Style(stripStyles(line), console.Dim)
	}
	return line
}

func truncateCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func stripStyles(line string) string {
	for {
		start := strings.Index(line, "\x1b[")
		if start < 0 {
			return line
		}
		end := strings.Index(line[start:], "m")
		if end < 0 {
			return line
		}
		line = line[:start] + line[start+end+1:]
	}
}

func deref(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func padCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return value + strings.Repeat(" ", width-len(runes))
}

func padLeft(value string, width int) string {
	if len([]rune(value)) >= width {
		return value
	}
	return strings.Repeat(" ", width-len([]rune(value))) + value
}
