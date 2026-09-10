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
	"sort"
	"syscall"
	"time"

	"github.com/gregasher/bedrock-local-proxy/internal/accounting"
	"github.com/gregasher/bedrock-local-proxy/internal/config"
	"github.com/gregasher/bedrock-local-proxy/internal/server"
)

var version = "dev"

type record struct {
	Event   string   `json:"event"`
	Message string   `json:"message,omitempty"`
	Profile string   `json:"profile,omitempty"`
	Region  string   `json:"region,omitempty"`
	Listen  string   `json:"listen,omitempty"`
	Models  []string `json:"models,omitempty"`
	Version string   `json:"version,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("bedrock-proxy", flag.ContinueOnError)
	// Keep the standard parser from writing before we can honor JSON mode.
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	logFormat := flags.String("log-format", "text", "log format: text or json")
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
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		diagnostic(stderr, *logFormat, fmt.Errorf("listen on %s: %w", cfg.Listen, err))
		return 1
	}
	srv := server.New(cfg)
	accountingFormat := accounting.FormatText
	if *logFormat == "json" {
		accountingFormat = accounting.FormatJSON
	}
	accountingRecorder := accounting.New(stdout, accountingFormat, cfg.Models)
	srv.SetCompletionRecorder(accountingRecorder.Record)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	if err := startup(stdout, *logFormat, cfg, ln.Addr().String()); err != nil {
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
		}
		accountingRecorder.MarkIncomplete(srv.PendingCompletions())
	case err := <-serveErr:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			diagnostic(stderr, *logFormat, err)
			return 1
		}
	}
	return 0
}

func requestedLogFormat(args []string) string {
	for i, arg := range args {
		if arg == "--log-format=json" || (arg == "--log-format" && i+1 < len(args) && args[i+1] == "json") {
			return "json"
		}
	}
	return "text"
}

func startup(stdout io.Writer, format string, cfg config.Config, address string) error {
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	if format == "json" {
		return json.NewEncoder(stdout).Encode(record{Event: "startup", Profile: cfg.AWS.Profile, Region: cfg.AWS.Region, Listen: "http://" + address + "/v1", Models: names})
	}
	fmt.Fprintln(stdout, "Bedrock Local Proxy")
	fmt.Fprintf(stdout, "Listening:   http://%s/v1\n", address)
	fmt.Fprintf(stdout, "AWS profile: %s\n", cfg.AWS.Profile)
	fmt.Fprintf(stdout, "Region:      %s\n", cfg.AWS.Region)
	fmt.Fprintf(stdout, "Models:      %s\n", join(names))
	return nil
}

func diagnostic(stderr io.Writer, format string, err error) {
	if format == "json" {
		_ = json.NewEncoder(stderr).Encode(record{Event: "error", Message: err.Error()})
		return
	}
	fmt.Fprintf(stderr, "bedrock-proxy: %s\n", err)
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
