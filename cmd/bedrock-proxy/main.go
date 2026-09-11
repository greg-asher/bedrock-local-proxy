package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/accounting"
	"github.com/gregasher/bedrock-local-proxy/internal/codex"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/doctor"
	"github.com/gregasher/bedrock-local-proxy/internal/reports"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

var version = "dev"
var codexCommandRunner codex.CommandRunner = codex.ExecRunner{}

type record struct {
	Event      string   `json:"event"`
	Message    string   `json:"message,omitempty"`
	Profile    string   `json:"profile,omitempty"`
	Region     string   `json:"region,omitempty"`
	Listen     string   `json:"listen,omitempty"`
	Models     []string `json:"models,omitempty"`
	Version    string   `json:"version,omitempty"`
	ReportPath string   `json:"report_path,omitempty"`
	SessionTag string   `json:"session_tag,omitempty"`
}

type singleValue struct {
	name  string
	set   bool
	value string
}

func (v *singleValue) String() string { return v.value }

func (v *singleValue) Set(value string) error {
	if v.set {
		return fmt.Errorf("--%s may only be provided once", v.name)
	}
	v.set = true
	v.value = value
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "report" {
		return runReport(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "configure" {
		if len(args) < 2 || args[1] != "codex" {
			diagnostic(stderr, "text", errors.New("configure requires the codex subcommand"))
			return 2
		}
		return runConfigureCodex(args[2:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:], stdout, stderr)
	}
	return runProxy(args, stdout, stderr)
}

func runConfigureCodex(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy configure codex", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	model := flags.String("model", "", "configured model alias")
	catalogPath := flags.String("catalog", "", "output path for the generated Codex model catalog")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if flags.NArg() != 0 {
		diagnostic(stderr, "text", errors.New("configure codex does not accept positional arguments"))
		return 2
	}
	path, cfg, err := loadConfiguration(*configPath)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	result, err := codex.Generate(context.Background(), codex.GenerateOptions{
		Config:       cfg,
		ConfigPath:   path,
		ModelAlias:   *model,
		CatalogPath:  *catalogPath,
		ProxyVersion: version,
		Runner:       codexCommandRunner,
	})
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	profilePath, err := doctor.DefaultProfilePath()
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	fmt.Fprintf(stdout, "Codex catalog: %s\n", result.CatalogPath)
	fmt.Fprintf(stdout, "Codex version: %s\n", result.CodexVersion)
	fmt.Fprintf(stdout, "Model:         %s\n\n", result.ModelAlias)
	for _, message := range result.Warnings {
		fmt.Fprintf(stderr, "WARNING: %s\n", message)
	}
	fmt.Fprintf(stdout, "Save this profile as %s:\n\n%s\n", profilePath, result.TOML)
	fmt.Fprintf(stdout, "Then run: codex --profile %s\n", codex.DefaultProfileName)
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	client := flags.String("client", "", "client compatibility checks: codex")
	model := flags.String("model", "", "configured model alias")
	live := flags.Bool("live", false, "exercise a running proxy and AWS target")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if flags.NArg() != 0 {
		diagnostic(stderr, "text", errors.New("doctor does not accept positional arguments"))
		return 2
	}
	if *live && strings.TrimSpace(*client) == "" {
		*client = "codex"
	}
	path, cfg, err := loadConfiguration(*configPath)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	result := doctor.Run(context.Background(), doctor.Options{
		Config:      cfg,
		ConfigPath:  path,
		Client:      *client,
		ModelAlias:  *model,
		Live:        *live,
		CodexRunner: codexCommandRunner,
	})
	for _, check := range result.Checks {
		fmt.Fprintf(stdout, "%-7s %-28s [%s] %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Category, check.Message)
	}
	if !result.OK() {
		return 1
	}
	return 0
}

func loadConfiguration(value string) (string, config.Config, error) {
	path := strings.TrimSpace(value)
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return "", config.Config{}, err
		}
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return "", config.Config{}, err
	}
	return path, cfg, nil
}

func runProxy(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy", flag.ContinueOnError)
	// Keep the standard parser from writing before we can honor JSON mode.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	logFormat := flags.String("log-format", "text", "log format: text or json")
	reportDir := flags.String("report-dir", "", "parent directory for per-session reports")
	var sessionTag singleValue
	sessionTag.name = "session-tag"
	flags.Var(&sessionTag, "session-tag", "optional ingestion tag for this session")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, requestedLogFormat(args), err)
		return 2
	}
	if *logFormat != "text" && *logFormat != "json" {
		diagnostic(stderr, *logFormat, errors.New("--log-format must be text or json"))
		return 2
	}
	if *showVersion {
		if *logFormat == "json" {
			_ = json.NewEncoder(stdout).Encode(record{Event: "version", Version: version})
		} else {
			fmt.Fprintf(stdout, "bedrock-proxy version %s\n", version)
		}
		return 0
	}
	if sessionTag.set {
		var err error
		sessionTag.value, err = accounting.NormalizeSessionTag(sessionTag.value)
		if err != nil {
			diagnostic(stderr, *logFormat, fmt.Errorf("invalid --session-tag: %w", err))
			return 2
		}
	}
	path := *configPath
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			diagnostic(stderr, *logFormat, err)
			return 1
		}
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		diagnostic(stderr, *logFormat, err)
		return 1
	}
	resolvedReportDir, err := config.ResolveReportDirectory(path, cfg.Reporting.Directory, *reportDir)
	if err != nil {
		diagnostic(stderr, *logFormat, err)
		return 1
	}
	sessionReporter, err := accounting.NewSessionReporter(accounting.SessionOptions{
		ParentDirectory: resolvedReportDir,
		SessionTag:      sessionTag.value,
		Version:         version,
		RequestedListen: cfg.Listen,
		Models:          cfg.Models,
	})
	if err != nil {
		diagnostic(stderr, *logFormat, fmt.Errorf("initialize session report: %w", err))
		return 1
	}
	sessionStatus := accounting.SessionStartupFailed
	defer func() { sessionReporter.Finalize(sessionStatus) }()
	if sessionReporter.HasCleanupWarning() {
		sessionReporter.Warn("retention_cleanup_failed")
		warning(stderr, *logFormat, "report retention cleanup failed")
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		sessionReporter.Warn("listen_failed")
		diagnostic(stderr, *logFormat, fmt.Errorf("listen on %s: %w", cfg.Listen, err))
		return 1
	}
	srv := server.New(cfg)
	accountingFormat := accounting.FormatText
	if *logFormat == "json" {
		accountingFormat = accounting.FormatJSON
	}
	accountingRecorder := accounting.New(stdout, accountingFormat, cfg.Models)
	accountingRecorder.SetSessionReporter(sessionReporter)
	srv.SetCompletionRecorder(accountingRecorder.Record)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	endpoint := "http://" + ln.Addr().String() + "/v1"
	if err := sessionReporter.Start(endpoint); err != nil {
		_ = ln.Close()
		diagnostic(stderr, *logFormat, fmt.Errorf("start session report: %w", err))
		return 1
	}
	if err := startup(stdout, *logFormat, cfg, ln.Addr().String(), sessionReporter.Info()); err != nil {
		_ = ln.Close()
		diagnostic(stderr, *logFormat, err)
		return 1
	}
	defer accountingRecorder.WriteSummary()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			diagnostic(stderr, *logFormat, fmt.Errorf("shutdown: %w", err))
			sessionStatus = accounting.SessionServerFailed
			return 1
		}
		accountingRecorder.MarkIncomplete(srv.PendingCompletions())
	case err := <-serveErr:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			diagnostic(stderr, *logFormat, err)
			sessionStatus = accounting.SessionServerFailed
			return 1
		}
	}
	sessionStatus = accounting.SessionCompleted
	return 0
}

func runReport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy report", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	startValue := flags.String("start", "", "inclusive RFC3339 start time")
	stopValue := flags.String("stop", "", "exclusive RFC3339 stop time")
	reportDir := flags.String("report-dir", "", "parent directory containing session reports")
	configPath := flags.String("config", "", "optional YAML configuration for reporting.directory")
	format := flags.String("format", "html", "report format: html or json")
	output := flags.String("output", "", "output path; defaults under the report directory")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if flags.NArg() != 0 {
		diagnostic(stderr, "text", fmt.Errorf("report does not accept positional arguments"))
		return 2
	}
	if *format != "html" && *format != "json" {
		diagnostic(stderr, "text", errors.New("--format must be html or json"))
		return 2
	}
	start, err := parseReportTime(*startValue)
	if err != nil {
		diagnostic(stderr, "text", fmt.Errorf("invalid --start: %w", err))
		return 2
	}
	stop, err := parseReportTime(*stopValue)
	if err != nil {
		diagnostic(stderr, "text", fmt.Errorf("invalid --stop: %w", err))
		return 2
	}
	directory, err := resolveReportCommandDirectory(*configPath, *reportDir)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	report, err := reports.Generate(reports.Options{Directory: directory, Start: start, Stop: stop})
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	var contents []byte
	if *format == "html" {
		contents, err = reports.HTML(report)
	} else {
		contents, err = json.MarshalIndent(report, "", "  ")
		if err == nil {
			contents = append(contents, '\n')
		}
	}
	if err != nil {
		diagnostic(stderr, "text", fmt.Errorf("render report: %w", err))
		return 1
	}
	path, err := reportOutputPath(*output, directory, start, stop, *format)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	if err := writeReport(path, contents); err != nil {
		diagnostic(stderr, "text", fmt.Errorf("write report: %w", err))
		return 1
	}
	fmt.Fprintf(stdout, "Report: %s\n", path)
	fmt.Fprintf(stdout, "Period: %s to %s UTC\n", report.Start, report.Stop)
	fmt.Fprintf(stdout, "Requests: %d · success rate: %s · estimated cost: $%.6f\n", report.Metrics.Requests, successRate(report.Metrics), report.Metrics.KnownEstimatedCost)
	return 0
}

func parseReportTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("is required")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, errors.New("use RFC3339 (for example 2026-09-01T00:00:00Z) or YYYY-MM-DD")
}

func resolveReportCommandDirectory(configPath, override string) (string, error) {
	if configPath == "" {
		if strings.TrimSpace(override) != "" {
			return config.ResolveReportDirectory("", "", override)
		}
		return config.DefaultReportDirectory()
	}
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return "", err
	}
	return config.ResolveReportDirectory(configPath, cfg.Reporting.Directory, override)
}

func reportOutputPath(value, directory string, start, stop time.Time, format string) (string, error) {
	if strings.TrimSpace(value) != "" {
		path, err := filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("resolve output path: %w", err)
		}
		return path, nil
	}
	name := fmt.Sprintf("report-%s-to-%s.%s", start.UTC().Format("20060102T150405Z"), stop.UTC().Format("20060102T150405Z"), format)
	return filepath.Join(directory, "summaries", name), nil
}

func writeReport(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
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

func successRate(metrics reports.Metrics) string {
	if metrics.Requests == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(metrics.Successes)*100/float64(metrics.Requests))
}

func requestedLogFormat(args []string) string {
	for i, arg := range args {
		if arg == "--log-format=json" || (arg == "--log-format" && i+1 < len(args) && args[i+1] == "json") {
			return "json"
		}
	}
	return "text"
}

func startup(stdout io.Writer, format string, cfg config.Config, address string, session accounting.SessionInfo) error {
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	if format == "json" {
		return json.NewEncoder(stdout).Encode(record{Event: "startup", Profile: cfg.AWS.Profile, Region: cfg.AWS.Region, Listen: "http://" + address + "/v1", Models: names, ReportPath: session.Directory, SessionTag: session.SessionTag})
	}
	fmt.Fprintln(stdout, "Bedrock Local Proxy")
	fmt.Fprintf(stdout, "Listening:   http://%s/v1\n", address)
	fmt.Fprintf(stdout, "AWS profile: %s\n", cfg.AWS.Profile)
	fmt.Fprintf(stdout, "Region:      %s\n", cfg.AWS.Region)
	fmt.Fprintf(stdout, "Models:      %s\n", join(names))
	fmt.Fprintf(stdout, "Session report: %s\n", session.Directory)
	if session.SessionTag != "" {
		fmt.Fprintf(stdout, "Session tag: %s\n", session.SessionTag)
	}
	return nil
}

func diagnostic(stderr io.Writer, format string, err error) {
	if format == "json" {
		_ = json.NewEncoder(stderr).Encode(record{Event: "error", Message: err.Error()})
		return
	}
	fmt.Fprintf(stderr, "bedrock-proxy: %s\n", err)
}

func warning(stderr io.Writer, format, message string) {
	if format == "json" {
		_ = json.NewEncoder(stderr).Encode(record{Event: "warning", Message: message})
		return
	}
	fmt.Fprintf(stderr, "bedrock-proxy: warning: %s\n", message)
}

func join(values []string) string {
	result := ""
	for i, value := range values {
		if i > 0 {
			result += ", "
		}
		result += value
	}
	return result
}
