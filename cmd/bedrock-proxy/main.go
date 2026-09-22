package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/accounting"
	"github.com/gregasher/bedrock-local-proxy/internal/claude"
	"github.com/gregasher/bedrock-local-proxy/internal/codex"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/console"
	"github.com/gregasher/bedrock-local-proxy/internal/doctor"
	"github.com/gregasher/bedrock-local-proxy/internal/reports"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

var version = "dev"
var codexCommandRunner codex.CommandRunner = codex.ExecRunner{}
var claudeCommandRunner claude.CommandRunner = claude.ExecRunner{}

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
	Preset     string   `json:"preset,omitempty"`
	Budget     *float64 `json:"budget,omitempty"`
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

// budgetValue parses --budget as an amount in the currency of the configured
// model prices. It rejects a repeated flag for the same reason singleValue
// does: two ceilings in one command line are ambiguous.
type budgetValue struct {
	set   bool
	value float64
}

func (v *budgetValue) String() string {
	if !v.set {
		return ""
	}
	return strconv.FormatFloat(v.value, 'f', -1, 64)
}

func (v *budgetValue) Set(value string) error {
	if v.set {
		return errors.New("--budget may only be provided once")
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "$")), 64)
	if err != nil {
		return fmt.Errorf("--budget must be an amount such as 2.50")
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return errors.New("--budget must be greater than zero")
	}
	v.set = true
	v.value = amount
	return nil
}

func main() {
	os.Exit(runWith(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, stdinIsTerminal()))
}

// stdinIsTerminal decides once whether prompting is appropriate. Interactivity
// is kept separate from the reader itself so the prompt loop can be exercised
// in tests with an ordinary reader and no pseudo-terminal.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// run keeps the non-interactive signature used by most callers and tests.
func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, strings.NewReader(""), stdout, stderr, false)
}

func runWith(args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	if len(args) > 0 && args[0] == "report" {
		return runReport(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "configure" {
		if len(args) < 2 {
			diagnostic(stderr, "text", errors.New("configure requires the codex or claude subcommand"))
			return 2
		}
		switch args[1] {
		case "codex":
			return runConfigureCodex(args[2:], stdout, stderr)
		case "claude":
			return runConfigureClaude(args[2:], stdout, stderr)
		default:
			diagnostic(stderr, "text", errors.New("configure requires the codex or claude subcommand"))
			return 2
		}
	}
	if len(args) > 0 && args[0] == "preset" {
		if len(args) < 2 || args[1] != "create" {
			diagnostic(stderr, "text", errors.New("preset requires the create subcommand"))
			return 2
		}
		return runPresetCreate(args[2:], stdin, stdout, stderr, interactive)
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:], stdout, stderr)
	}
	return runProxy(args, stdout, stderr)
}

func runConfigureClaude(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy configure claude", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	model := flags.String("model", "", "override the configured Claude default model")
	settingsPath := flags.String("settings", "", "output path for generated Claude Code settings")
	var preset singleValue
	preset.name = "preset"
	flags.Var(&preset, "preset", "named preset from the configuration's presets block")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if flags.NArg() != 0 {
		diagnostic(stderr, "text", errors.New("configure claude does not accept positional arguments"))
		return 2
	}
	loaded, err := loadConfiguration(*configPath, preset.value)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	result, err := claude.Generate(context.Background(), claude.GenerateOptions{
		Config:       loaded.effective,
		ConfigPath:   loaded.path,
		ModelAlias:   *model,
		SettingsPath: *settingsPath,
		Preset:       loaded.preset,
		Runner:       claudeCommandRunner,
	})
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	fmt.Fprintf(stdout, "Claude settings: %s\n", result.SettingsPath)
	fmt.Fprintf(stdout, "Claude version: %s\n", result.ClaudeVersion)
	fmt.Fprintf(stdout, "Default model: %s\n", result.ModelAlias)
	fmt.Fprintf(stdout, "Subagent model: %s\n", result.SubagentAlias)
	fmt.Fprintf(stdout, "Picker models: %s\n", strings.Join(result.ModelAliases, ", "))
	if len(result.SkippedModels) != 0 {
		fmt.Fprintln(stdout, "Skipped models:")
		for _, skipped := range result.SkippedModels {
			fmt.Fprintf(stdout, "  %s: %s\n", skipped.Alias, skipped.Reason)
		}
	}
	for _, message := range result.Warnings {
		fmt.Fprintf(stderr, "WARNING: %s\n", message)
	}
	for _, skipped := range result.SkippedModels {
		fmt.Fprintf(stderr, "WARNING: skipped model %s: %s\n", skipped.Alias, skipped.Reason)
	}
	fmt.Fprintf(stdout, "Then run: claude --settings %s\n", shellQuote(result.SettingsPath))
	return 0
}

func shellQuote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("_@%+=:,./-", r)
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runConfigureCodex(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy configure codex", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	model := flags.String("model", "", "override the configured Codex default model")
	catalogPath := flags.String("catalog", "", "output path for the generated Codex model catalog")
	var preset singleValue
	preset.name = "preset"
	flags.Var(&preset, "preset", "named preset from the configuration's presets block")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if flags.NArg() != 0 {
		diagnostic(stderr, "text", errors.New("configure codex does not accept positional arguments"))
		return 2
	}
	loaded, err := loadConfiguration(*configPath, preset.value)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	result, err := codex.Generate(context.Background(), codex.GenerateOptions{
		Config:       loaded.effective,
		ConfigPath:   loaded.path,
		ModelAlias:   *model,
		CatalogPath:  *catalogPath,
		Preset:       loaded.preset,
		ProxyVersion: version,
		Runner:       codexCommandRunner,
	})
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	profilePath, err := doctor.DefaultProfilePath(loaded.preset)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	fmt.Fprintf(stdout, "Codex catalog: %s\n", result.CatalogPath)
	fmt.Fprintf(stdout, "Codex version: %s\n", result.CodexVersion)
	fmt.Fprintf(stdout, "Default model: %s\n", result.ModelAlias)
	fmt.Fprintf(stdout, "Catalog models: %s\n", strings.Join(result.ModelAliases, ", "))
	if len(result.SkippedModels) != 0 {
		fmt.Fprintln(stdout, "Skipped models:")
		for _, skipped := range result.SkippedModels {
			fmt.Fprintf(stdout, "  %s: %s\n", skipped.Alias, skipped.Reason)
		}
	}
	fmt.Fprintln(stdout)
	for _, message := range result.Warnings {
		fmt.Fprintf(stderr, "WARNING: %s\n", message)
	}
	for _, skipped := range result.SkippedModels {
		fmt.Fprintf(stderr, "WARNING: skipped model %s: %s\n", skipped.Alias, skipped.Reason)
	}
	fmt.Fprintf(stdout, "Save this profile as %s:\n\n%s\n", profilePath, result.TOML)
	fmt.Fprintf(stdout, "Then run: codex --profile %s\n", result.ProfileName)
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	client := flags.String("client", "", "client compatibility checks: codex or claude")
	model := flags.String("model", "", "override the configured client default model")
	settingsPath := flags.String("settings", "", "path to generated Claude Code settings")
	live := flags.Bool("live", false, "exercise a running proxy and AWS target")
	var preset singleValue
	preset.name = "preset"
	flags.Var(&preset, "preset", "named preset from the configuration's presets block")
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
	loaded, err := loadConfiguration(*configPath, preset.value)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	result := doctor.Run(context.Background(), doctor.Options{
		Config:             loaded.effective,
		ConfigPath:         loaded.path,
		Preset:             loaded.preset,
		Client:             *client,
		ModelAlias:         *model,
		ClaudeSettingsPath: *settingsPath,
		Live:               *live,
		CodexRunner:        codexCommandRunner,
		ClaudeRunner:       claudeCommandRunner,
	})
	for _, check := range result.Checks {
		fmt.Fprintf(stdout, "%-7s %-28s [%s] %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Category, check.Message)
	}
	if !result.OK() {
		return 1
	}
	return 0
}

// loadedConfig carries both views a subcommand may need: the file as written,
// which holds the full model catalog and every preset, and the effective
// configuration a preset resolves to.
type loadedConfig struct {
	path      string
	catalog   config.Config
	effective config.Config
	preset    string
}

func loadConfiguration(value, preset string) (loadedConfig, error) {
	path := strings.TrimSpace(value)
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return loadedConfig{}, err
		}
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return loadedConfig{}, err
	}
	preset = strings.TrimSpace(preset)
	effective, err := cfg.Resolve(preset)
	if err != nil {
		return loadedConfig{}, err
	}
	return loadedConfig{path: path, catalog: cfg, effective: effective, preset: preset}, nil
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
	var preset singleValue
	preset.name = "preset"
	flags.Var(&preset, "preset", "named preset from the configuration's presets block")
	var listen singleValue
	listen.name = "listen"
	flags.Var(&listen, "listen", "loopback address to bind, overriding the configured listen")
	var budget budgetValue
	flags.Var(&budget, "budget", "refuse generation requests once this run's estimated cost reaches this amount")
	colorFlag := flags.String("color", "auto", "colorize text output: auto, always, or never")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, requestedLogFormat(args), err)
		return 2
	}
	if *logFormat != "text" && *logFormat != "json" {
		diagnostic(stderr, *logFormat, errors.New("--log-format must be text or json"))
		return 2
	}
	colorMode, err := console.ParseColorMode(*colorFlag)
	if err != nil {
		diagnostic(stderr, *logFormat, err)
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
	if listen.set {
		listen.value = strings.TrimSpace(listen.value)
		if err := config.ValidateListenAddress(listen.value); err != nil {
			diagnostic(stderr, *logFormat, fmt.Errorf("invalid --listen: %w", err))
			return 2
		}
	}
	loaded, err := loadConfiguration(*configPath, preset.value)
	if err != nil {
		diagnostic(stderr, *logFormat, err)
		return 1
	}
	path, cfg := loaded.path, loaded.effective
	// --listen replaces whichever address the configuration resolved to, the
	// top-level one or a preset's. A single run binds one address, so the
	// port-uniqueness check across presets has nothing left to say here.
	if listen.set {
		cfg.Listen = listen.value
	}
	// A preset supplies the session tag; --session-tag overrides it for one
	// run, the same way --report-dir overrides reporting.directory.
	if !sessionTag.set {
		sessionTag.value = loaded.catalog.SessionTag(loaded.preset)
	}
	resolvedReportDir, err := config.ResolveReportDirectory(path, cfg.Reporting.Directory, *reportDir)
	if err != nil {
		diagnostic(stderr, *logFormat, err)
		return 1
	}
	sessionReporter, err := accounting.NewSessionReporter(accounting.SessionOptions{
		ParentDirectory: resolvedReportDir,
		SessionTag:      sessionTag.value,
		Preset:          loaded.preset,
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
		// Ports within a configuration are checked when it loads, so a bind
		// failure here means something outside this configuration holds the
		// address, not another preset.
		diagnostic(stderr, *logFormat, fmt.Errorf("listen on %s: %w; this configuration's own ports do not overlap, so another process is using this address", cfg.Listen, err))
		return 1
	}
	srv := server.New(cfg)
	accountingFormat := accounting.FormatText
	if *logFormat == "json" {
		accountingFormat = accounting.FormatJSON
	}
	out := console.New(stdout, colorMode)
	accountingRecorder := accounting.New(stdout, accountingFormat, cfg.Models)
	if accountingFormat == accounting.FormatText {
		accountingRecorder.SetConsole(out)
	}
	accountingRecorder.SetSessionReporter(sessionReporter)
	srv.SetCompletionRecorder(accountingRecorder.Record)
	if budget.set {
		srv.SetBudget(budget.value, accountingRecorder.BudgetSpend)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	endpoint := "http://" + ln.Addr().String() + "/v1"
	if err := sessionReporter.Start(endpoint); err != nil {
		_ = ln.Close()
		diagnostic(stderr, *logFormat, fmt.Errorf("start session report: %w", err))
		return 1
	}
	budgetLimit := (*float64)(nil)
	if budget.set {
		budgetLimit = &budget.value
	}
	if err := startup(stdout, out, *logFormat, cfg, ln.Addr().String(), sessionReporter.Info(), budgetLimit); err != nil {
		_ = ln.Close()
		diagnostic(stderr, *logFormat, err)
		return 1
	}
	if accountingFormat == accounting.FormatText {
		out.StartStatus(accountingRecorder.Status)
	}
	// The footer is torn down before the summary so the closing block is the
	// last thing left on screen.
	defer func() {
		out.Close()
		accountingRecorder.WriteSummary()
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			// Tear the footer down first: a diagnostic printed over it would
			// be erased with it.
			out.Close()
			diagnostic(stderr, *logFormat, fmt.Errorf("shutdown: %w", err))
			sessionStatus = accounting.SessionServerFailed
			return 1
		}
		accountingRecorder.MarkIncomplete(srv.PendingCompletions())
	case err := <-serveErr:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			out.Close()
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
	configPath := flags.String("config", "", "YAML configuration for report directory and current model pricing")
	format := flags.String("format", "html", "report format: html or json")
	output := flags.String("output", "", "output path; defaults under the report directory")
	reprice := flags.Bool("reprice", false, "deprecated; reports always use current configuration prices")
	var preset singleValue
	preset.name = "preset"
	flags.Var(&preset, "preset", "limit the report to sessions recorded under this preset")
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
	_ = reprice
	directory, pricingModels, configured, err := resolveReportCommandInputs(*configPath, *reportDir, preset.value)
	if err == nil && !configured {
		err = errors.New("report requires a usable selected or default configuration for current prices")
	}
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}
	report, err := reports.Generate(reports.Options{Directory: directory, Start: start, Stop: stop, Models: pricingModels, Preset: preset.value, Reprice: true})
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
	if preset.value != "" {
		fmt.Fprintf(stdout, "Preset: %s\n", preset.value)
	}
	fmt.Fprintf(stdout, "Period: %s to %s UTC\n", report.Start, report.Stop)
	fmt.Fprintf(stdout, "Requests: %d · success rate: %s · estimated cost: %s\n", report.Metrics.Requests, successRate(report.Metrics), periodCostSummary(report.Metrics))
	fmt.Fprintf(stdout, "Pricing: current prices · priced: %d · unavailable: %d\n", report.Metrics.RepricedCostRequests, report.Metrics.MissingCostRequests)
	for _, issue := range report.PricingIssues {
		fmt.Fprintf(stdout, "Pricing issue: model=%s · requests=%d · %s\n", issue.Model, issue.Requests, issue.Reason)
	}
	return 0
}

func periodCostSummary(metrics reports.Metrics) string {
	if metrics.Requests == 0 {
		return "n/a"
	}
	if metrics.MissingCostRequests >= metrics.Requests {
		return "unavailable"
	}
	value := fmt.Sprintf("$%.6f", metrics.KnownEstimatedCost)
	if metrics.MissingCostRequests > 0 {
		return value + " (partial)"
	}
	return value
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

// resolveReportCommandInputs returns the report directory and the pricing
// catalog. The catalog is deliberately the unfiltered top-level models map: a
// preset narrows which sessions are reported, never which prices are known.
// Pricing a historical request against a preset's subset would silently report
// its cost as unavailable.
func resolveReportCommandInputs(configPath, override, preset string) (string, map[string]config.ModelConfig, bool, error) {
	path := strings.TrimSpace(configPath)
	explicit := path != ""
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return "", nil, false, err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			directory, resolveErr := config.ResolveReportDirectory("", "", override)
			return directory, nil, false, resolveErr
		} else if err != nil {
			return "", nil, false, fmt.Errorf("inspect config %q: %w", path, err)
		}
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		if explicit {
			return "", nil, false, err
		}
		return "", nil, false, fmt.Errorf("load default configuration for report pricing: %w", err)
	}
	if preset = strings.TrimSpace(preset); preset != "" {
		if _, err := cfg.Resolve(preset); err != nil {
			return "", nil, false, err
		}
	}
	directory, err := config.ResolveReportDirectory(path, cfg.Reporting.Directory, override)
	if err != nil {
		return "", nil, false, err
	}
	return directory, cfg.Models, true, nil
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

func startup(stdout io.Writer, out *console.Console, format string, cfg config.Config, address string, session accounting.SessionInfo, budget *float64) error {
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	if format == "json" {
		return json.NewEncoder(stdout).Encode(record{Event: "startup", Profile: cfg.AWS.Profile, Region: cfg.AWS.Region, Listen: "http://" + address + "/v1", Models: names, ReportPath: session.Directory, SessionTag: session.SessionTag, Preset: session.Preset, Budget: budget})
	}
	out.Banner(console.Banner{
		Version:    version,
		Address:    "http://" + address + "/v1",
		Tag:        session.SessionTag,
		Preset:     session.Preset,
		Profile:    cfg.AWS.Profile,
		Region:     cfg.AWS.Region,
		ReportPath: session.Directory,
		Models:     names,
		Budget:     budget,
	})
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
