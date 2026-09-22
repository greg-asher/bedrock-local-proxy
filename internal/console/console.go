// Package console renders the proxy's human-readable output. It never
// receives prompt or header content, only the same metadata the accounting
// records already carry, and it degrades to plain text whenever the output is
// not an interactive terminal.
package console

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ColorMode selects styling independently of terminal detection, so a run
// piped into a pager can still be colored and a terminal can be kept plain.
type ColorMode string

const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

func ParseColorMode(value string) (ColorMode, error) {
	switch ColorMode(strings.TrimSpace(value)) {
	case ColorAuto:
		return ColorAuto, nil
	case ColorAlways:
		return ColorAlways, nil
	case ColorNever:
		return ColorNever, nil
	}
	return "", fmt.Errorf("--color must be auto, always, or never")
}

type Style uint8

const (
	Plain Style = iota
	Dim
	Bold
	Green
	Red
	Yellow
	Cyan
)

var codes = map[Style]string{Dim: "2", Bold: "1", Green: "32", Red: "31", Yellow: "33", Cyan: "36"}

// Span is one styled run of text. Box content is assembled from spans so that
// padding can be measured on the visible text, never on escape sequences.
type Span struct {
	Text  string
	Style Style
}

// Banner is the startup identity of one run. The console keeps it so the
// closing summary can restate the budget and report path without the caller
// threading them through accounting.
type Banner struct {
	Version    string
	Address    string
	Tag        string
	Preset     string
	Profile    string
	Region     string
	ReportPath string
	Models     []string
	Budget     *float64
}

// Status is the live footer state, pulled from accounting on every refresh.
type Status struct {
	Requests int
	Failures int
	Spent    float64
	Uptime   time.Duration
}

// Metrics is the closing summary, mirroring the accounting totals.
type Metrics struct {
	Runtime                       time.Duration
	Requests, Successes, Failures int
	InputTokens, OutputTokens     int64
	CacheReadTokens               int64
	CacheWriteTokens              int64
	EstimatedCost                 float64
	CostStatus                    string
	MissingUsage                  int
	MissingEstimates              int
	Incomplete                    int
}

// Console serializes every write so that request lines, the footer, and the
// refresh ticker cannot interleave mid-line.
type Console struct {
	mu        sync.Mutex
	w         io.Writer
	color     bool
	rich      bool
	width     int
	banner    Banner
	file      *os.File
	status    func() Status
	footer    bool
	announced float64
	stop      chan struct{}
	stopped   chan struct{}
}

// New detects an interactive terminal from the writer itself, so a test
// buffer, a redirected file, and a pipe all produce plain output without the
// caller having to say so.
func New(w io.Writer, mode ColorMode) *Console {
	terminal := isTerminal(w)
	color := terminal
	switch mode {
	case ColorAlways:
		color = true
	case ColorNever:
		color = false
	}
	if os.Getenv("NO_COLOR") != "" && mode != ColorAlways {
		color = false
	}
	file, _ := w.(*os.File)
	c := &Console{w: w, color: color, rich: terminal, file: file}
	c.refreshWidth()
	return c
}

// refreshWidth re-measures before each rendered block, so resizing the window
// mid-run reflows the footer and the boxes instead of leaving a stale width.
func (c *Console) refreshWidth() {
	c.width = defaultWidth
	if value, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && value >= minWidth && value <= maxWidth {
		c.width = value
		return
	}
	if c.file != nil {
		if measured := queryTerminalWidth(c.file); measured >= minWidth && measured <= maxWidth {
			c.width = measured
		}
	}
}

// Rich reports whether the output is an interactive terminal. Only a terminal
// gets box drawing and the live footer; everything else stays line-oriented so
// redirected logs remain greppable.
func (c *Console) Rich() bool { return c.rich }

// Width returns the current terminal width. Plain output returns zero because
// redirected logs should keep every field instead of adapting to a terminal.
func (c *Console) Width() int {
	if !c.rich {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshWidth()
	return c.width
}

func (c *Console) Style(text string, style Style) string {
	code, ok := codes[style]
	if !c.color || !ok || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// Glyph marks an outcome. The terminal gets a symbol; plain output gets a word
// that survives grep.
func (c *Console) Glyph(ok bool) string {
	if !c.rich {
		if ok {
			return c.Style("ok ", Green)
		}
		return c.Style("ERR", Red)
	}
	if ok {
		return c.Style(" ✓ ", Green)
	}
	return c.Style(" ✗ ", Red)
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

const (
	defaultWidth = 80
	minWidth     = 40
	maxWidth     = 400
)
