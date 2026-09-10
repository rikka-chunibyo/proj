package main

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

const progressBarWidth = 24

type relocationProgress struct {
	enabled   bool
	w         io.Writer
	prefix    string
	total     int64
	copied    int64
	started   time.Time
	lastDraw  time.Time
	lastWidth int
	liveLine  bool
}

func newRelocationProgress(enabled bool, w io.Writer, operation, name string) *relocationProgress {
	verb := map[string]string{"import": "Importing", "archive": "Archiving", "restore": "Restoring"}[operation]
	if verb == "" {
		verb = "Moving"
	}
	return &relocationProgress{enabled: enabled, w: w, prefix: verb + " " + name}
}

func (p *relocationProgress) Status(message string) {
	if !p.enabled {
		return
	}
	p.CleanLine()
	fmt.Fprintf(p.w, "%s  %s\n", p.prefix, message)
}

func (p *relocationProgress) BeginCopy(total int64) {
	if !p.enabled {
		return
	}
	p.total = total
	p.copied = 0
	p.started = time.Now()
	p.lastDraw = time.Time{}
	p.render(true)
}

func (p *relocationProgress) AddBytes(n int64) {
	if !p.enabled || n <= 0 {
		return
	}
	p.copied += n
	p.render(false)
}

func (p *relocationProgress) FinishCopy() {
	if !p.enabled {
		return
	}
	p.render(true)
	fprintNewline(p.w)
	p.liveLine = false
	p.lastWidth = 0
}

func (p *relocationProgress) CleanLine() {
	if !p.enabled || !p.liveLine {
		return
	}
	fprintPadding(p.w, p.lastWidth)
	fprintNewline(p.w)
	p.liveLine = false
	p.lastWidth = 0
}

func (p *relocationProgress) render(force bool) {
	if !p.enabled {
		return
	}
	now := time.Now()
	if !force && !p.lastDraw.IsZero() && now.Sub(p.lastDraw) < 100*time.Millisecond {
		return
	}
	p.lastDraw = now
	elapsed := now.Sub(p.started).Seconds()
	rate := float64(0)
	if elapsed > 0 {
		rate = float64(p.copied) / elapsed
	}
	percent := 100
	if p.total > 0 {
		percent = int(float64(p.copied) * 100 / float64(p.total))
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
	}
	filled := progressBarWidth * percent / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", progressBarWidth-filled)
	line := fmt.Sprintf("%s  [%s]  %3d%%  %s / %s", p.prefix, bar, percent, humanBytes(p.copied), humanBytes(p.total))
	if rate > 0 {
		line += "  " + humanBytes(int64(rate)) + "/s"
		if p.total > p.copied {
			seconds := int64(math.Ceil(float64(p.total-p.copied) / rate))
			line += "  ETA " + compactDuration(time.Duration(seconds)*time.Second)
		} else {
			line += "  ETA 0s"
		}
	}
	padding := ""
	lineWidth := utf8.RuneCountInString(line)
	if lineWidth < p.lastWidth {
		padding = strings.Repeat(" ", p.lastWidth-lineWidth)
	}
	fmt.Fprintf(p.w, "\r%s%s", line, padding)
	p.lastWidth = lineWidth
	p.liveLine = true
}

func fprintPadding(w io.Writer, width int) {
	if width > 0 {
		fmt.Fprint(w, "\r", strings.Repeat(" ", width), "\r")
	}
}

func fprintNewline(w io.Writer) { fmt.Fprint(w, "\n") }

func humanBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, suffix := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.2f EiB", value/unit)
}

func compactDuration(d time.Duration) string {
	seconds := int64(math.Ceil(d.Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds %= 60
	if minutes < 60 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	hours := minutes / 60
	minutes %= 60
	return fmt.Sprintf("%dh%02dm", hours, minutes)
}
