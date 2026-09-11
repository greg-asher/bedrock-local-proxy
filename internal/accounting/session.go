package accounting

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

const (
	reportSchemaVersion = 1
	reportRetention     = 30 * 24 * time.Hour
)

// SessionStatus records the terminal state of a proxy execution. Running is
// intentionally retained after an abrupt process exit, where finalization
// cannot run.
type SessionStatus string

const (
	SessionRunning       SessionStatus = "running"
	SessionCompleted     SessionStatus = "completed"
	SessionStartupFailed SessionStatus = "startup_failed"
	SessionServerFailed  SessionStatus = "server_failed"
)

// SessionOptions supplies only metadata that is safe to persist in a report.
type SessionOptions struct {
	ParentDirectory string
	SessionTag      string
	Version         string
	RequestedListen string
	Models          map[string]config.ModelConfig
}

// SessionInfo is the safe startup metadata exposed by the command.
type SessionInfo struct {
	Directory  string
	SessionID  string
	SessionTag string
}

type sessionModel struct {
	Alias            string `json:"alias"`
	BedrockModelID   string `json:"bedrock_model_id"`
	MetadataProfile  string `json:"metadata_profile,omitempty"`
	MetadataRevision string `json:"metadata_revision,omitempty"`
	ContextWindow    int64  `json:"context_window,omitempty"`
	MaxOutputTokens  int64  `json:"max_output_tokens,omitempty"`
}

type sessionManifest struct {
	SchemaVersion   int            `json:"schema_version"`
	SessionID       string         `json:"session_id"`
	SessionTag      string         `json:"session_tag,omitempty"`
	Status          SessionStatus  `json:"status"`
	StartedAt       string         `json:"started_at"`
	FinishedAt      string         `json:"finished_at,omitempty"`
	Version         string         `json:"version,omitempty"`
	ReportPath      string         `json:"report_path"`
	RequestedListen string         `json:"requested_listen,omitempty"`
	Listen          string         `json:"listen,omitempty"`
	Models          []sessionModel `json:"models"`
}

type startupEvent struct {
	Event      string         `json:"event"`
	Timestamp  string         `json:"timestamp"`
	SessionID  string         `json:"session_id"`
	SessionTag string         `json:"session_tag,omitempty"`
	ReportPath string         `json:"report_path"`
	Listen     string         `json:"listen"`
	Models     []sessionModel `json:"models"`
}

type warningEvent struct {
	Event      string `json:"event"`
	Timestamp  string `json:"timestamp"`
	SessionID  string `json:"session_id"`
	SessionTag string `json:"session_tag,omitempty"`
	Code       string `json:"code"`
}

// SessionReporter writes one private report directory. It never receives
// request payloads or credentials, only the accounting metadata types.
type SessionReporter struct {
	mu             sync.Mutex
	directory      string
	events         *os.File
	manifest       sessionManifest
	cleanupWarning bool
	summary        *summaryRecord
}

// NormalizeSessionTag validates and trims the optional ingestion tag.
func NormalizeSessionTag(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("session tag must be valid UTF-8")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("session tag must not be empty")
	}
	if len([]byte(value)) > 128 {
		return "", fmt.Errorf("session tag must be at most 128 UTF-8 bytes")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("session tag must not contain control characters")
		}
	}
	return value, nil
}

// NewSessionReporter initializes report storage before the proxy binds its
// listener. An initialization failure is returned so callers can fail closed.
func NewSessionReporter(options SessionOptions) (*SessionReporter, error) {
	if strings.TrimSpace(options.ParentDirectory) == "" {
		return nil, fmt.Errorf("report directory is required")
	}
	if options.SessionTag != "" {
		tag, err := NormalizeSessionTag(options.SessionTag)
		if err != nil {
			return nil, err
		}
		options.SessionTag = tag
	}
	parent, err := filepath.Abs(options.ParentDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve report directory: %w", err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create report directory: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return nil, fmt.Errorf("secure report directory: %w", err)
	}

	started := time.Now().UTC()
	suffix, err := randomSuffix()
	if err != nil {
		return nil, fmt.Errorf("create report identifier: %w", err)
	}
	id := fmt.Sprintf("%s-%d-%s", started.Format("20060102T150405.000000000Z"), os.Getpid(), suffix)
	directory := filepath.Join(parent, id)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create session report: %w", err)
	}

	reporter := &SessionReporter{directory: directory}
	reporter.manifest = sessionManifest{
		SchemaVersion:   reportSchemaVersion,
		SessionID:       id,
		SessionTag:      options.SessionTag,
		Status:          SessionRunning,
		StartedAt:       started.Format(time.RFC3339Nano),
		Version:         options.Version,
		ReportPath:      directory,
		RequestedListen: options.RequestedListen,
		Models:          reportModels(options.Models),
	}
	if err := writeJSONAtomic(filepath.Join(directory, "session.json"), reporter.manifest); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("write session manifest: %w", err)
	}
	events, err := os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("create session events: %w", err)
	}
	reporter.events = events
	if cleanupExpiredReports(parent, directory, started) {
		reporter.cleanupWarning = true
	}
	return reporter, nil
}

func (r *SessionReporter) Info() SessionInfo {
	return SessionInfo{Directory: r.directory, SessionID: r.manifest.SessionID, SessionTag: r.manifest.SessionTag}
}

// Start marks the listener as available and writes the startup event.
func (r *SessionReporter) Start(listen string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifest.Status = SessionRunning
	r.manifest.Listen = listen
	if err := writeJSONAtomic(filepath.Join(r.directory, "session.json"), r.manifest); err != nil {
		return err
	}
	return r.writeEventLocked(startupEvent{Event: "startup", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), SessionID: r.manifest.SessionID, SessionTag: r.manifest.SessionTag, ReportPath: r.directory, Listen: listen, Models: r.manifest.Models})
}

func (r *SessionReporter) HasCleanupWarning() bool { return r.cleanupWarning }

// Warn appends only a stable code, never raw OS or upstream errors.
func (r *SessionReporter) Warn(code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.writeEventLocked(warningEvent{Event: "warning", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), SessionID: r.manifest.SessionID, SessionTag: r.manifest.SessionTag, Code: code})
}

func (r *SessionReporter) RecordRequest(entry requestRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry.SessionID = r.manifest.SessionID
	entry.SessionTag = r.manifest.SessionTag
	_ = r.writeEventLocked(entry)
}

// StageSummary receives the accounting totals before finalization. The report
// is written later, together with the definitive lifecycle status.
func (r *SessionReporter) StageSummary(summary summaryRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summary = &summary
}

// Finalize writes the final manifest atomically. If the process is terminated
// before this call, the initial running manifest deliberately remains.
func (r *SessionReporter) Finalize(status SessionStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifest.Status = status
	r.manifest.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	summary := summaryRecord{Event: "summary", CostStatus: "unavailable"}
	if r.summary != nil {
		summary = *r.summary
	}
	summary.SessionID = r.manifest.SessionID
	summary.SessionTag = r.manifest.SessionTag
	summary.StartedAt = r.manifest.StartedAt
	summary.FinishedAt = r.manifest.FinishedAt
	summary.Status = string(status)
	if err := writeJSONAtomic(filepath.Join(r.directory, "summary.json"), summary); err == nil {
		_ = r.writeEventLocked(summary)
	}
	_ = writeJSONAtomic(filepath.Join(r.directory, "session.json"), r.manifest)
	if r.events != nil {
		_ = r.events.Sync()
		_ = r.events.Close()
		r.events = nil
	}
}

func (r *SessionReporter) writeEventLocked(value any) error {
	if r.events == nil {
		return io.ErrClosedPipe
	}
	if err := json.NewEncoder(r.events).Encode(value); err != nil {
		return err
	}
	return nil
}

func reportModels(models map[string]config.ModelConfig) []sessionModel {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]sessionModel, 0, len(names))
	for _, name := range names {
		model := models[name]
		entry := sessionModel{Alias: name, BedrockModelID: model.BedrockModelID}
		if model.Capabilities != nil {
			if capabilities, _, err := config.ResolveModelCapabilities(model); err == nil {
				entry.MetadataProfile = capabilities.MetadataProfile
				entry.MetadataRevision = capabilities.MetadataRevision
				entry.ContextWindow = capabilities.ContextWindow
				entry.MaxOutputTokens = capabilities.MaxOutputTokens
			}
		}
		result = append(result, entry)
	}
	return result
}

func randomSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeJSONAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// cleanupExpiredReports removes only direct child directories that contain a
// valid report manifest. It deliberately skips links, malformed manifests, and
// all unrecognized directories. true means startup should show a safe warning.
func cleanupExpiredReports(parent, current string, now time.Time) (warning bool) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return true
	}
	cutoff := now.Add(-reportRetention)
	for _, entry := range entries {
		path := filepath.Join(parent, entry.Name())
		if path == current || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			warning = true
			continue
		}
		manifestPath := filepath.Join(path, "session.json")
		manifestInfo, err := os.Lstat(manifestPath)
		if err != nil {
			if !os.IsNotExist(err) {
				warning = true
			}
			continue
		}
		if manifestInfo.Mode()&os.ModeSymlink != 0 || !manifestInfo.Mode().IsRegular() {
			continue
		}
		bytes, err := os.ReadFile(manifestPath)
		if err != nil {
			warning = true
			continue
		}
		var manifest sessionManifest
		if json.Unmarshal(bytes, &manifest) != nil || !validManifest(manifest, path) {
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, manifest.StartedAt)
		if err != nil {
			continue
		}
		if started.Before(cutoff) && os.RemoveAll(path) != nil {
			warning = true
		}
	}
	return warning
}

func validManifest(manifest sessionManifest, directory string) bool {
	return manifest.SchemaVersion == reportSchemaVersion && manifest.SessionID == filepath.Base(directory) && filepath.Clean(manifest.ReportPath) == filepath.Clean(directory) && manifest.StartedAt != ""
}
