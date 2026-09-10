package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHumanBytesAndDuration(t *testing.T) {
	for _, tc := range []struct {
		bytes int64
		want  string
	}{{0, "0 B"}, {1023, "1023 B"}, {1024, "1.00 KiB"}, {1024 * 1024, "1.00 MiB"}, {3 * 1024 * 1024 * 1024, "3.00 GiB"}} {
		if got := humanBytes(tc.bytes); got != tc.want {
			t.Errorf("humanBytes(%d)=%q, want %q", tc.bytes, got, tc.want)
		}
	}
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{8 * time.Second, "8s"}, {65 * time.Second, "1m05s"}, {2*time.Hour + 3*time.Minute, "2h03m"}} {
		if got := compactDuration(tc.d); got != tc.want {
			t.Errorf("compactDuration(%s)=%q", tc.d, got)
		}
	}
}

func TestProgressLineHasRequiredFieldsAndNoANSI(t *testing.T) {
	var out bytes.Buffer
	p := newRelocationProgress(true, &out, "archive", "Rune")
	p.total = 255 * 1024 * 1024
	p.copied = 184 * 1024 * 1024
	p.started = time.Now().Add(-2 * time.Second)
	p.render(true)
	text := out.String()
	for _, want := range []string{"Archiving Rune", "[", "72%", "184.00 MiB / 255.00 MiB", "/s", "ETA"} {
		if !strings.Contains(text, want) {
			t.Fatalf("progress missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatalf("ANSI emitted: %q", text)
	}
	p.CleanLine()
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("line not cleaned: %q", out.String())
	}
}

func TestCrossFilesystemProgressAndPhases(t *testing.T) {
	a, root := testApp(t)
	a.ProgressTTY = true
	a.RelocationHooks.ForceCopy = true
	source := filepath.Join(root, "outside", "Progress")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "payload.bin"), strings.Repeat("p", 1024*1024), 0o644)
	if err := a.Run([]string{"import", source, "--yes"}); err != nil {
		t.Fatal(err)
	}
	progress := errString(a)
	for _, want := range []string{"Importing Progress  Scanning source...", "[", "100%", "1.00 MiB / 1.00 MiB", "/s", "ETA 0s", "Verifying source snapshot...", "Finalizing destination...", "Updating registry...", "Checking source before cleanup...", "Removing verified old source...", "Regenerating generated views..."} {
		if !strings.Contains(progress, want) {
			t.Fatalf("missing %q in progress:\n%q", want, progress)
		}
	}
	if strings.Contains(progress, "\x1b") {
		t.Fatal("progress emitted ANSI escapes")
	}
}

func TestNonTTYRelocationHasNoProgressControls(t *testing.T) {
	a, root := testApp(t)
	a.RelocationHooks.ForceCopy = true
	source := filepath.Join(root, "outside", "Quiet")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "x.txt"), "x", 0o644)
	if err := a.Run([]string{"import", source, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := errString(a); strings.ContainsAny(got, "\r\x1b") || strings.Contains(got, "Scanning source") || strings.Contains(got, "Regenerating generated views") {
		t.Fatalf("non-TTY progress leaked: %q", got)
	}
}

func TestSameFilesystemUsesStatusButNoProgressBar(t *testing.T) {
	a, root := testApp(t)
	a.ProgressTTY = true
	source := filepath.Join(root, "outside", "Atomic")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "x.txt"), "x", 0o644)
	if err := a.Run([]string{"import", source, "--yes"}); err != nil {
		t.Fatal(err)
	}
	status := errString(a)
	if !strings.Contains(status, "Moving atomically on the same filesystem") || !strings.Contains(status, "Updating registry") || !strings.Contains(status, "Regenerating generated views") {
		t.Fatal(status)
	}
	if strings.Contains(status, "[") {
		t.Fatalf("same-filesystem move showed progress bar: %q", status)
	}
}

func TestCopyFailureLeavesTTYOnCleanLine(t *testing.T) {
	a, root := testApp(t)
	a.ProgressTTY = true
	a.RelocationHooks.ForceCopy = true
	a.RelocationHooks.CopyTree = func(string, string) error { return syscall.ENOSPC }
	source := filepath.Join(root, "outside", "Failure")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "x.txt"), "x", 0o644)
	err := a.Run([]string{"import", source, "--yes"})
	if err == nil || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("copy failure: %v", err)
	}
	status := errString(a)
	if !strings.HasSuffix(status, "\n") {
		t.Fatalf("terminal line left open: %q", status)
	}
	if strings.Contains(status, "\x1b") {
		t.Fatal("failure path emitted ANSI escapes")
	}
	if _, statErr := os.Stat(filepath.Join(source, "x.txt")); statErr != nil {
		t.Fatal("failure weakened source preservation")
	}
}
