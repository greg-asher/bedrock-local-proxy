package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

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
}

func main() {
	configPath := flag.String("config", "", "path to YAML configuration")
	logFormat := flag.String("log-format", "text", "log format: text or json")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("bedrock-proxy version %s\n", version)
		return
	}
	if *logFormat != "text" && *logFormat != "json" {
		diagnostic(*logFormat, errors.New("--log-format must be text or json"))
		os.Exit(2)
	}
	path := *configPath
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			diagnostic(*logFormat, err)
			os.Exit(1)
		}
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		diagnostic(*logFormat, err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		diagnostic(*logFormat, fmt.Errorf("listen on %s: %w", cfg.Listen, err))
		os.Exit(1)
	}
	srv := server.New(cfg)
	if err := startup(*logFormat, cfg); err != nil {
		_ = ln.Close()
		diagnostic(*logFormat, err)
		os.Exit(1)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	select {
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			diagnostic(*logFormat, fmt.Errorf("shutdown: %w", err))
		}
	case err := <-serveErr:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			diagnostic(*logFormat, err)
			os.Exit(1)
		}
	}
}

func startup(format string, cfg config.Config) error {
	names := make([]string, 0, len(cfg.Models))
	for name := range cfg.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	if format == "json" {
		return json.NewEncoder(os.Stdout).Encode(record{Event: "startup", Profile: cfg.AWS.Profile, Region: cfg.AWS.Region, Listen: "http://" + cfg.Listen + "/v1", Models: names})
	}
	fmt.Fprintln(os.Stdout, "Bedrock Local Proxy")
	fmt.Fprintf(os.Stdout, "Listening:   http://%s/v1\n", cfg.Listen)
	fmt.Fprintf(os.Stdout, "AWS profile: %s\n", cfg.AWS.Profile)
	fmt.Fprintf(os.Stdout, "Region:      %s\n", cfg.AWS.Region)
	fmt.Fprintf(os.Stdout, "Models:      %s\n", join(names))
	return nil
}

func diagnostic(format string, err error) {
	if format == "json" {
		_ = json.NewEncoder(os.Stderr).Encode(record{Event: "error", Message: err.Error()})
		return
	}
	fmt.Fprintf(os.Stderr, "bedrock-proxy: %s\n", err)
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
