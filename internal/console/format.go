package console

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Money renders an amount in the currency of the configured model prices.
// Small amounts keep more places, because a single request often costs less
// than a cent and rounding it to $0.00 would hide the whole point of a budget.
func Money(value float64) string {
	switch {
	case value == 0:
		return "$0"
	case value < 0.01:
		return fmt.Sprintf("$%.6f", value)
	case value < 1:
		return fmt.Sprintf("$%.4f", value)
	default:
		return fmt.Sprintf("$%.2f", value)
	}
}

// Tokens abbreviates counts so the token columns stay the same width as
// traffic grows.
func Tokens(value int64) string {
	switch {
	case value < 0:
		return "-"
	case value < 1000:
		return fmt.Sprintf("%d", value)
	case value < 1_000_000:
		return trimZero(float64(value)/1000) + "k"
	default:
		return trimZero(float64(value)/1_000_000) + "M"
	}
}

func trimZero(value float64) string {
	text := fmt.Sprintf("%.1f", value)
	return strings.TrimSuffix(text, ".0")
}

// Duration renders elapsed time at one useful precision instead of Go's
// default nanosecond tail.
func Duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return trimZero(d.Seconds()) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// Bar draws a proportion. It is clamped so an overrun still renders a full
// bar rather than spilling across the line.
func Bar(fraction float64, width int) string {
	if width <= 0 {
		return ""
	}
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(fraction*float64(width) + 0.5)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// ShortenPath makes a report path readable without hiding which session it
// is: the home prefix collapses to ~, and an over-long remainder keeps its
// head and its identifying tail.
func ShortenPath(path string, width int) string {
	if path == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "/" && strings.HasPrefix(path, home+string(os.PathSeparator)) {
		path = "~" + path[len(home):]
	}
	if width < 24 {
		width = 24
	}
	runes := []rune(path)
	if len(runes) <= width {
		return path
	}
	// Keep at most half the budget for the trailing session id, so the head
	// still says which directory tree the report landed in.
	tail := width / 2
	if index := strings.LastIndex(path, string(os.PathSeparator)); index >= 0 {
		if name := []rune(path[index+1:]); len(name) < tail {
			tail = len(name)
		}
	}
	return string(runes[:width-tail-2]) + "…/" + string(runes[len(runes)-tail:])
}
