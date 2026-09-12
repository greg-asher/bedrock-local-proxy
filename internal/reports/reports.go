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
	GeneratedAt      string      `json:"generated_at"`
	ReportDirectory  string      `json:"report_directory"`
	Start            string      `json:"start"`
	Stop             string      `json:"stop"`
	Sessions         int         `json:"sessions"`
	SkippedSessions  int         `json:"skipped_sessions"`
	Metrics          Metrics     `json:"metrics"`
	Series           []Point     `json:"series"`
	Models           []Breakdown `json:"models"`
	Endpoints        []Breakdown `json:"endpoints"`
	SessionTags      []Breakdown `json:"session_tags"`
	Clients          []Breakdown `json:"clients"`
	MetadataProfiles []Breakdown `json:"metadata_profiles"`
	Catalogs         []Breakdown `json:"catalogs"`
	ClaudeSettings   []Breakdown `json:"claude_settings"`
}

type Metrics struct {
	Requests                     int     `json:"requests"`
	Successes                    int     `json:"successes"`
	Failures                     int     `json:"failures"`
	Canceled                     int     `json:"canceled"`
	ModelListEvents              int     `json:"model_list_events"`
	KnownInputTokens             int64   `json:"known_input_tokens"`
	KnownOutputTokens            int64   `json:"known_output_tokens"`
	KnownCacheReadInputTokens    int64   `json:"known_cache_read_input_tokens"`
	KnownCacheWriteInputTokens   int64   `json:"known_cache_write_input_tokens"`
	KnownEstimatedCost           float64 `json:"known_estimated_cost"`
	MissingUsageRequests         int     `json:"missing_usage_requests"`
	MissingCostRequests          int     `json:"missing_cost_requests"`
	FunctionToolCalls            int     `json:"function_tool_calls"`
	UnsupportedFeatureRejections int     `json:"unsupported_feature_rejections"`
	AverageContextUtilization    float64 `json:"average_context_utilization"`
	AverageOutputUtilization     float64 `json:"average_output_utilization"`
	ContextUtilizationSamples    int     `json:"context_utilization_samples"`
	OutputUtilizationSamples     int     `json:"output_utilization_samples"`
}

type Point struct {
	Start                        string  `json:"start"`
	Requests                     int     `json:"requests"`
	Successes                    int     `json:"successes"`
	Failures                     int     `json:"failures"`
	Canceled                     int     `json:"canceled"`
	KnownEstimatedCost           float64 `json:"known_estimated_cost"`
	MissingCostRequests          int     `json:"missing_cost_requests"`
	FunctionToolCalls            int     `json:"function_tool_calls"`
	UnsupportedFeatureRejections int     `json:"unsupported_feature_rejections"`
}

type Breakdown struct {
	Name                         string  `json:"name"`
	Requests                     int     `json:"requests"`
	Successes                    int     `json:"successes"`
	Failures                     int     `json:"failures"`
	Canceled                     int     `json:"canceled"`
	KnownInputTokens             int64   `json:"known_input_tokens"`
	KnownOutputTokens            int64   `json:"known_output_tokens"`
	KnownCacheReadInputTokens    int64   `json:"known_cache_read_input_tokens"`
	KnownCacheWriteInputTokens   int64   `json:"known_cache_write_input_tokens"`
	KnownEstimatedCost           float64 `json:"known_estimated_cost"`
	MissingCostRequests          int     `json:"missing_cost_requests"`
	FunctionToolCalls            int     `json:"function_tool_calls"`
	UnsupportedFeatureRejections int     `json:"unsupported_feature_rejections"`
}

type manifest struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	ReportPath    string `json:"report_path"`
}

type event struct {
	Event                        string   `json:"event"`
	Timestamp                    string   `json:"timestamp"`
	SessionTag                   string   `json:"session_tag"`
	Endpoint                     string   `json:"endpoint"`
	LocalModel                   string   `json:"local_model"`
	Outcome                      string   `json:"outcome"`
	InputTokens                  *int64   `json:"input_tokens"`
	OutputTokens                 *int64   `json:"output_tokens"`
	CacheReadInputTokens         *int64   `json:"cache_read_input_tokens"`
	CacheWriteInputTokens        *int64   `json:"cache_write_input_tokens"`
	EstimatedCost                *float64 `json:"estimated_cost"`
	UsageStatus                  string   `json:"usage_status"`
	CostStatus                   string   `json:"cost_status"`
	ClientFamily                 string   `json:"client_family"`
	ClientVersion                string   `json:"client_version"`
	MetadataProfile              string   `json:"metadata_profile"`
	MetadataRevision             string   `json:"metadata_revision"`
	CatalogHash                  string   `json:"catalog_hash"`
	SettingsHash                 string   `json:"settings_hash"`
	ContextUtilization           *float64 `json:"context_utilization"`
	OutputUtilization            *float64 `json:"output_utilization"`
	FunctionToolCalls            int      `json:"function_tool_calls"`
	UnsupportedFeatureRejections int      `json:"unsupported_feature_rejections"`
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
	byClient := map[string]*Breakdown{}
	byProfile := map[string]*Breakdown{}
	byCatalog := map[string]*Breakdown{}
	byClaudeSettings := map[string]*Breakdown{}
	var contextTotal, outputTotal float64
	var contextSamples, outputSamples int
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
			if strings.HasPrefix(item.Endpoint, "/v1/models") {
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
			applyBreakdown(byClient, clientLabel(item), item)
			applyBreakdown(byProfile, profileLabel(item), item)
			applyBreakdown(byCatalog, label(item.CatalogHash, "no catalog marker"), item)
			applyBreakdown(byClaudeSettings, label(item.SettingsHash, "no Claude settings marker"), item)
			if item.ContextUtilization != nil {
				contextTotal += *item.ContextUtilization
				contextSamples++
			}
			if item.OutputUtilization != nil {
				outputTotal += *item.OutputUtilization
				outputSamples++
			}
		}
	}
	report.Sessions = len(contributingSessions)
	report.Series = orderedPoints(byBucket)
	report.Models = orderedBreakdowns(byModel)
	report.Endpoints = orderedBreakdowns(byEndpoint)
	report.SessionTags = orderedBreakdowns(byTag)
	report.Clients = orderedBreakdowns(byClient)
	report.MetadataProfiles = orderedBreakdowns(byProfile)
	report.Catalogs = orderedBreakdowns(byCatalog)
	report.ClaudeSettings = orderedBreakdowns(byClaudeSettings)
	if contextSamples > 0 {
		report.Metrics.AverageContextUtilization = contextTotal / float64(contextSamples)
	}
	if outputSamples > 0 {
		report.Metrics.AverageOutputUtilization = outputTotal / float64(outputSamples)
	}
	report.Metrics.ContextUtilizationSamples = contextSamples
	report.Metrics.OutputUtilizationSamples = outputSamples
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
	if item.CacheReadInputTokens != nil {
		metrics.KnownCacheReadInputTokens += *item.CacheReadInputTokens
	}
	if item.CacheWriteInputTokens != nil {
		metrics.KnownCacheWriteInputTokens += *item.CacheWriteInputTokens
	}
	if item.UsageStatus != "known" {
		metrics.MissingUsageRequests++
	}
	if item.EstimatedCost != nil {
		metrics.KnownEstimatedCost += *item.EstimatedCost
	} else {
		metrics.MissingCostRequests++
	}
	metrics.FunctionToolCalls += item.FunctionToolCalls
	metrics.UnsupportedFeatureRejections += item.UnsupportedFeatureRejections
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
	} else {
		point.MissingCostRequests++
	}
	point.FunctionToolCalls += item.FunctionToolCalls
	point.UnsupportedFeatureRejections += item.UnsupportedFeatureRejections
}

func clientLabel(item event) string {
	if strings.TrimSpace(item.ClientFamily) == "" {
		return "unknown client"
	}
	if strings.TrimSpace(item.ClientVersion) == "" {
		return item.ClientFamily
	}
	return item.ClientFamily + " " + item.ClientVersion
}

func profileLabel(item event) string {
	if strings.TrimSpace(item.MetadataProfile) == "" {
		return "explicit or legacy metadata"
	}
	if strings.TrimSpace(item.MetadataRevision) == "" {
		return item.MetadataProfile
	}
	return item.MetadataProfile + " @ " + item.MetadataRevision
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
	if item.CacheReadInputTokens != nil {
		value.KnownCacheReadInputTokens += *item.CacheReadInputTokens
	}
	if item.CacheWriteInputTokens != nil {
		value.KnownCacheWriteInputTokens += *item.CacheWriteInputTokens
	}
	if item.EstimatedCost != nil {
		value.KnownEstimatedCost += *item.EstimatedCost
	} else {
		value.MissingCostRequests++
	}
	value.FunctionToolCalls += item.FunctionToolCalls
	value.UnsupportedFeatureRejections += item.UnsupportedFeatureRejections
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
