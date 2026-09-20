package console

import (
	"fmt"
	"strings"
	"time"
)

const (
	minBoxWidth = 44
	maxBoxWidth = 78
	// minFramedWidth is the narrowest terminal whose frames still contain the
	// widest summary row.
	minFramedWidth = 72
)

// Banner prints the startup identity. The endpoint, session tag, and budget
// are framed because they are what an operator looks for first; the rest is
// dimmed detail below the frame.
func (c *Console) Banner(b Banner) {
	c.mu.Lock()
	c.banner = b
	c.mu.Unlock()

	title := "Bedrock Local Proxy"
	highlights := []Span{{Text: b.Address, Style: Cyan}}
	var facts []Span
	if b.Tag != "" {
		facts = append(facts, Span{Text: "tag ", Style: Dim}, Span{Text: b.Tag + "   ", Style: Bold})
	}
	if b.Preset != "" {
		facts = append(facts, Span{Text: "preset ", Style: Dim}, Span{Text: b.Preset + "   ", Style: Bold})
	}
	if b.Budget != nil {
		facts = append(facts, Span{Text: "budget ", Style: Dim}, Span{Text: Money(*b.Budget), Style: Yellow})
	}
	lines := [][]Span{highlights}
	if len(facts) > 0 {
		lines = append(lines, facts)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeBox(title, b.Version, lines)
	c.writeDetail("aws", b.Profile+" · "+b.Region)
	c.writeDetail("models", strings.Join(b.Models, ", "))
	c.writeDetail("report", ShortenPath(b.ReportPath, c.width-12))
}

func (c *Console) writeDetail(label, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(c.w, "  %s %s\n", c.Style(pad(label, 8), Dim), c.Style(value, Dim))
}

// Print emits one log line above the footer. When there is no footer, it also
// carries the budget threshold announcements a redirected log would otherwise
// never see.
func (c *Console) Print(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseFooterLocked()
	fmt.Fprintln(c.w, line)
	if !c.footer {
		c.announceThresholdLocked()
	}
	c.drawFooterLocked()
}

// StartStatus turns on the live footer. It is a terminal-only affordance:
// redrawing in place would corrupt a redirected log, so other writers get
// threshold lines from Print instead.
func (c *Console) StartStatus(status func() Status) {
	if status == nil {
		return
	}
	c.mu.Lock()
	c.status = status
	if !c.rich {
		c.mu.Unlock()
		return
	}
	c.stop, c.stopped = make(chan struct{}), make(chan struct{})
	stop, stopped := c.stop, c.stopped
	c.drawFooterLocked()
	c.mu.Unlock()
	// The footer carries uptime, so it refreshes on a timer rather than only
	// when a request happens to complete.
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				c.mu.Lock()
				c.eraseFooterLocked()
				c.drawFooterLocked()
				c.mu.Unlock()
			}
		}
	}()
}

// Close removes the footer and stops its refresh. It is safe to call more
// than once.
func (c *Console) Close() {
	c.mu.Lock()
	stop, stopped := c.stop, c.stopped
	c.stop, c.stopped = nil, nil
	c.eraseFooterLocked()
	c.status = nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
}

func (c *Console) drawFooterLocked() {
	if !c.rich || c.status == nil {
		return
	}
	c.refreshWidth()
	// The trailing carriage return parks the cursor at column 0. Anything
	// else writing to this terminal (a stderr diagnostic, the http server's
	// default error log) then overwrites the footer instead of being
	// appended to it and wiped by the next erase.
	fmt.Fprint(c.w, c.footerText(c.status())+"\r")
	c.footer = true
}

func (c *Console) eraseFooterLocked() {
	if !c.footer {
		return
	}
	fmt.Fprint(c.w, "\r\x1b[2K")
	c.footer = false
}

// footerText renders the pinned status bar. It is truncated rather than
// wrapped: a wrapped footer would leave an orphan line behind on every
// redraw.
// segment is one footer field with the rank that decides what survives a
// narrow terminal. Higher keeps.
type segment struct {
	text string
	rank int
}

func (c *Console) footerText(s Status) string {
	segments := []segment{}
	if c.banner.Tag != "" {
		segments = append(segments, segment{c.banner.Tag, 6})
	}
	if port := portOf(c.banner.Address); port != "" {
		segments = append(segments, segment{":" + port, 7})
	}
	segments = append(segments, segment{fmt.Sprintf("%d req", s.Requests), 5})
	if s.Failures > 0 {
		segments = append(segments, segment{fmt.Sprintf("%d failed", s.Failures), 8})
	}
	segments = append(segments, segment{Money(s.Spent), 9})
	if c.banner.Budget != nil {
		used := 0.0
		if *c.banner.Budget > 0 {
			used = s.Spent / *c.banner.Budget
		}
		remaining := *c.banner.Budget - s.Spent
		if remaining < 0 {
			remaining = 0
		}
		segments = append(segments,
			segment{Bar(used, 12), 3},
			segment{fmt.Sprintf("%.0f%%", used*100), 4},
			// The whole point of the footer: never dropped.
			segment{Money(remaining) + " left", 10})
	}
	segments = append(segments, segment{Duration(s.Uptime), 1})

	// Drop the least useful field until the bar fits, rather than slicing a
	// line that would then wrap and leave an orphan on every redraw.
	for {
		text := " " + strings.Join(texts(segments), " · ") + " "
		if len([]rune(text)) <= c.width-1 || len(segments) == 1 {
			if runes := []rune(text); len(runes) > c.width-1 {
				text = string(runes[:c.width-1])
			}
			if !c.color {
				return text
			}
			return "\x1b[7m" + text + "\x1b[0m"
		}
		segments = dropLowest(segments)
	}
}

func texts(segments []segment) []string {
	values := make([]string, 0, len(segments))
	for _, s := range segments {
		values = append(values, s.text)
	}
	return values
}

func dropLowest(segments []segment) []segment {
	lowest := 0
	for i, s := range segments {
		if s.rank < segments[lowest].rank {
			lowest = i
		}
	}
	return append(segments[:lowest:lowest], segments[lowest+1:]...)
}

// Summary prints the closing block, restating the budget from the banner so
// the run ends with the number the operator started it for.
func (c *Console) Summary(m Metrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eraseFooterLocked()
	c.refreshWidth()

	success := "n/a"
	if m.Requests > 0 {
		success = fmt.Sprintf("%d (%.1f%%)", m.Successes, float64(m.Successes)*100/float64(m.Requests))
	}
	failures := Span{Text: fmt.Sprintf("%d", m.Failures)}
	if m.Failures > 0 {
		failures.Style = Red
	}
	lines := [][]Span{{
		{Text: pad("requests", 10), Style: Dim}, {Text: pad(fmt.Sprintf("%d", m.Requests), 10), Style: Bold},
		{Text: "success ", Style: Dim}, {Text: pad(success, 14)},
		{Text: "failed ", Style: Dim}, failures,
	}, {
		{Text: pad("tokens", 10), Style: Dim},
		{Text: pad("in "+Tokens(m.InputTokens), 13)},
		{Text: pad("out "+Tokens(m.OutputTokens), 13)},
		{Text: "cache ", Style: Dim},
		{Text: fmt.Sprintf("r %s · w %s", Tokens(m.CacheReadTokens), Tokens(m.CacheWriteTokens))},
	}}

	cost := []Span{{Text: pad("cost", 10), Style: Dim}, {Text: pad(costText(m), 13), Style: Yellow}}
	if c.banner.Budget != nil {
		used := 0.0
		if *c.banner.Budget > 0 {
			used = m.EstimatedCost / *c.banner.Budget
		}
		cost = append(cost,
			Span{Text: "budget ", Style: Dim}, Span{Text: pad(Money(*c.banner.Budget), 11)},
			Span{Text: Bar(used, 12)}, Span{Text: fmt.Sprintf(" %.0f%% used", used*100), Style: Dim})
	}
	lines = append(lines, cost)
	if gaps := gapText(m); gaps != "" {
		lines = append(lines, []Span{{Text: pad("gaps", 10), Style: Dim}, {Text: gaps, Style: Yellow}})
	}
	if c.banner.ReportPath != "" {
		lines = append(lines, []Span{{Text: pad("report", 10), Style: Dim}, {Text: ShortenPath(c.banner.ReportPath, c.boxWidth()-16), Style: Dim}})
	}
	c.writeBox("Session summary", Duration(m.Runtime), lines)
}

func costText(m Metrics) string {
	switch m.CostStatus {
	case "estimated":
		return Money(m.EstimatedCost)
	case "partial":
		return Money(m.EstimatedCost) + " (partial)"
	}
	return "unavailable"
}

// gapText names only the coverage that is actually missing, so a clean run
// shows no line at all.
func gapText(m Metrics) string {
	var parts []string
	if m.MissingUsage > 0 {
		parts = append(parts, fmt.Sprintf("%d without usage", m.MissingUsage))
	}
	if m.MissingEstimates > 0 {
		parts = append(parts, fmt.Sprintf("%d unpriced", m.MissingEstimates))
	}
	if m.Incomplete > 0 {
		parts = append(parts, fmt.Sprintf("%d incomplete", m.Incomplete))
	}
	return strings.Join(parts, " · ")
}

// announceThresholdLocked emits a budget notice at 50/75/90/100% for writers
// that cannot show a footer. Each level is announced at most once.
func (c *Console) announceThresholdLocked() {
	if c.status == nil || c.banner.Budget == nil || *c.banner.Budget <= 0 {
		return
	}
	used := c.status().Spent / *c.banner.Budget
	for _, level := range []float64{1, 0.9, 0.75, 0.5} {
		if used >= level && c.announced < level {
			c.announced = level
			remaining := *c.banner.Budget - c.status().Spent
			if remaining < 0 {
				remaining = 0
			}
			fmt.Fprintf(c.w, "%s budget %.0f%% used · %s of %s remaining\n",
				c.Style("!", Yellow), level*100, Money(remaining), Money(*c.banner.Budget))
			return
		}
	}
}

// writeBox frames content on a terminal and falls back to indented lines
// elsewhere, keeping redirected output free of drawing characters.
func (c *Console) writeBox(title, trailer string, lines [][]Span) {
	c.refreshWidth()
	// Below this the fixed-width cells no longer fit between the frames, and
	// a framed box would render ragged, so plain output is the honest choice.
	if !c.rich || c.width < minFramedWidth {
		fmt.Fprintf(c.w, "%s\n", c.Style(title, Bold))
		for _, line := range lines {
			fmt.Fprintf(c.w, "  %s\n", c.render(line))
		}
		return
	}
	inner := c.boxWidth()

	head := "─ " + title + " "
	if trailer != "" {
		trailer = " " + trailer + " ─"
	}
	fill := inner - runeLen(head) - runeLen(trailer)
	if fill < 0 {
		fill = 0
	}
	frame := func(text string) string { return c.Style(text, Dim) }
	fmt.Fprintf(c.w, "%s\n", frame("╭"+head+strings.Repeat("─", fill)+trailer+"╮"))
	for _, line := range lines {
		gap := inner - visibleWidth(line) - 4
		if gap < 0 {
			gap = 0
		}
		fmt.Fprintf(c.w, "%s  %s%s  %s\n", frame("│"), c.render(line), strings.Repeat(" ", gap), frame("│"))
	}
	fmt.Fprintf(c.w, "%s\n", frame("╰"+strings.Repeat("─", inner)+"╯"))
}

// boxWidth is deliberately independent of content: the banner and the closing
// summary are read as one frame around the run, so they must share an edge.
func (c *Console) boxWidth() int {
	return clamp(maxBoxWidth, minBoxWidth, max(c.width-2, minBoxWidth))
}

func (c *Console) render(spans []Span) string {
	var b strings.Builder
	for _, span := range spans {
		b.WriteString(c.Style(span.Text, span.Style))
	}
	return b.String()
}

func visibleWidth(spans []Span) int {
	total := 0
	for _, span := range spans {
		total += runeLen(span.Text)
	}
	return total
}

func runeLen(value string) int { return len([]rune(value)) }

func clamp(value, low, high int) int {
	if high < low {
		return low
	}
	return min(max(value, low), high)
}

func portOf(address string) string {
	if index := strings.LastIndex(address, ":"); index >= 0 {
		return strings.TrimSuffix(address[index+1:], "/v1")
	}
	return ""
}

func pad(value string, width int) string {
	if runeLen(value) >= width {
		return value + " "
	}
	return value + strings.Repeat(" ", width-runeLen(value))
}
