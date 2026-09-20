package console

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNonTerminalWriterGetsPlainUnframedOutput(t *testing.T) {
	var out bytes.Buffer
	c := New(&out, ColorAuto)
	budget := 5.0
	c.Banner(Banner{Address: "http://127.0.0.1:8787/v1", Tag: "ingest", Budget: &budget, Profile: "p", Region: "us-east-2", Models: []string{"coding"}})
	c.StartStatus(func() Status { return Status{Requests: 1} })
	c.Print("a line")
	c.Close()
	text := out.String()
	if strings.Contains(text, "\x1b[") {
		t.Errorf("escape sequences in redirected output: %q", text)
	}
	for _, unwanted := range []string{"╭", "│", "╰"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("box drawing in redirected output: %q", text)
		}
	}
	if !strings.Contains(text, "ingest") || !strings.Contains(text, "$5.00") || !strings.Contains(text, "a line") {
		t.Errorf("output = %q", text)
	}
}

// Without a footer there is no other place for the budget to appear, so a
// redirected log must still be told when it is running out.
func TestThresholdsAnnouncedWhenNoFooterIsPossible(t *testing.T) {
	var out bytes.Buffer
	c := New(&out, ColorNever)
	budget := 10.0
	c.Banner(Banner{Address: "http://127.0.0.1:1/v1", Budget: &budget})
	spent := 0.0
	c.StartStatus(func() Status { return Status{Spent: spent} })
	spent = 5.5
	c.Print("one")
	spent = 9.6
	c.Print("two")
	c.Print("three")
	text := out.String()
	if strings.Count(text, "budget 50% used") != 1 {
		t.Errorf("50%% threshold not announced exactly once:\n%s", text)
	}
	if !strings.Contains(text, "budget 90% used") {
		t.Errorf("90%% threshold missing:\n%s", text)
	}
	if strings.Contains(text, "budget 75% used") {
		t.Errorf("a level that was jumped over should not be announced:\n%s", text)
	}
}

func TestFooterDropsLowValueFieldsBeforeTruncating(t *testing.T) {
	c := &Console{w: &bytes.Buffer{}, rich: true, width: 60}
	budget := 5.0
	c.banner = Banner{Tag: "nightly-ingest", Address: "http://127.0.0.1:18801/v1", Budget: &budget}
	text := c.footerText(Status{Requests: 42, Failures: 3, Spent: 1.25, Uptime: 4 * time.Minute})
	if len([]rune(text)) > 59 {
		t.Fatalf("footer overflows the terminal: %q", text)
	}
	if !strings.Contains(text, "left") || !strings.Contains(text, "18801") {
		t.Errorf("footer dropped something it must keep: %q", text)
	}
}

func TestFooterKeepsEveryFieldOnAWideTerminal(t *testing.T) {
	c := &Console{w: &bytes.Buffer{}, rich: true, width: 160}
	budget := 5.0
	c.banner = Banner{Tag: "nightly-ingest", Address: "http://127.0.0.1:18801/v1", Budget: &budget}
	text := c.footerText(Status{Requests: 42, Failures: 3, Spent: 1.25, Uptime: 4 * time.Minute})
	for _, want := range []string{"nightly-ingest", ":18801", "42 req", "3 failed", "$1.25", "█", "25%", "$3.75 left", "4m 00s"} {
		if !strings.Contains(text, want) {
			t.Errorf("wide footer missing %q: %q", want, text)
		}
	}
}

func TestMoneyKeepsSubCentAmountsVisible(t *testing.T) {
	for _, c := range []struct {
		value float64
		want  string
	}{{0, "$0"}, {0.0000021, "$0.000002"}, {0.25, "$0.2500"}, {12.5, "$12.50"}} {
		if got := Money(c.value); got != c.want {
			t.Errorf("Money(%v) = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestTokensAndDurationStayNarrow(t *testing.T) {
	for _, c := range []struct {
		value int64
		want  string
	}{{0, "0"}, {999, "999"}, {1500, "1.5k"}, {1_000_000, "1M"}, {1_240_000, "1.2M"}} {
		if got := Tokens(c.value); got != c.want {
			t.Errorf("Tokens(%d) = %q, want %q", c.value, got, c.want)
		}
	}
	for _, c := range []struct {
		value time.Duration
		want  string
	}{{500 * time.Microsecond, "500µs"}, {12 * time.Millisecond, "12ms"}, {1500 * time.Millisecond, "1.5s"}, {252 * time.Second, "4m 12s"}} {
		if got := Duration(c.value); got != c.want {
			t.Errorf("Duration(%v) = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestParseColorModeRejectsUnknownValues(t *testing.T) {
	if _, err := ParseColorMode("rainbow"); err == nil {
		t.Fatal("expected an error for an unknown color mode")
	}
	for _, value := range []string{"auto", " always ", "never"} {
		if _, err := ParseColorMode(value); err != nil {
			t.Errorf("ParseColorMode(%q) = %v", value, err)
		}
	}
}

// A footer that left the cursor mid-line would have any concurrent stderr
// write appended to it, and the next erase would wipe both.
func TestFooterParksCursorAtColumnZero(t *testing.T) {
	var out bytes.Buffer
	c := &Console{w: &out, rich: true, width: 80, color: false}
	c.banner = Banner{Tag: "t", Address: "http://127.0.0.1:8787/v1"}
	c.status = func() Status { return Status{Requests: 1} }
	c.drawFooterLocked()
	if !strings.HasSuffix(out.String(), "\r") {
		t.Fatalf("footer must end at column 0, got %q", out.String())
	}
}

func TestNarrowTerminalFallsBackToUnframedOutput(t *testing.T) {
	var out bytes.Buffer
	c := &Console{w: &out, rich: true, width: 50, color: false}
	c.file = nil
	t.Setenv("COLUMNS", "50")
	budget := 5.0
	c.banner = Banner{Budget: &budget, ReportPath: "/Users/x/.local/state/bedrock-proxy/sessions/20260920T143249Z-9567"}
	c.Summary(Metrics{Runtime: time.Minute, Requests: 42, Successes: 40, Failures: 2, EstimatedCost: 3.14, CostStatus: "estimated"})
	if strings.Contains(out.String(), "│") {
		t.Fatalf("a terminal too narrow to frame must not draw ragged boxes:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "requests") {
		t.Fatalf("summary content missing:\n%s", out.String())
	}
}

func TestWideTerminalStillFrames(t *testing.T) {
	var out bytes.Buffer
	t.Setenv("COLUMNS", "100")
	c := &Console{w: &out, rich: true, color: false}
	c.refreshWidth()
	c.Summary(Metrics{Runtime: time.Minute, Requests: 1, Successes: 1, CostStatus: "unavailable"})
	if !strings.Contains(out.String(), "╭") {
		t.Fatalf("a wide terminal should frame the summary:\n%s", out.String())
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if width := len([]rune(line)); width != maxBoxWidth+2 {
			t.Errorf("box line width = %d, want %d: %q", width, maxBoxWidth+2, line)
		}
	}
}
