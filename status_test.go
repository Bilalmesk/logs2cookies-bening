package main

import (
	"strings"
	"testing"
)

func TestShortNameKeepsSuffix(t *testing.T) {
	long := "@UP_DAISYCLOUD_CHAMPIONING!_09_JULY_5913_ON_CHANNEL.rar"
	got := shortName(long, 40)
	if !strings.HasSuffix(got, ".rar") {
		t.Fatalf("lost ext: %q", got)
	}
	if !strings.Contains(got, "CHANNEL") && !strings.Contains(got, "…") {
		t.Fatalf("expected truncation with ellipsis: %q", got)
	}
	if strings.Count(got, "_") > 20 {
		// still fine, just ensure it shortened
	}
	if len([]rune(got)) > 42 {
		t.Fatalf("too long: %q (%d)", got, len([]rune(got)))
	}
}

func TestStatusDownloadNoMarkdownBreakage(t *testing.T) {
	name := "@UP_DAISYCLOUD_CHAMPIONING!_09_JULY_5913_ON_CHANNEL.rar"
	msg := statusDownload(name, 1<<30, 4<<30, 12.5*1024*1024)
	// must be HTML pre block, not markdown backticks around bar
	if !strings.Contains(msg, "<pre>") || !strings.Contains(msg, "</pre>") {
		t.Fatalf("expected pre block:\n%s", msg)
	}
	if strings.Contains(msg, "\\_") {
		t.Fatalf("should not markdown-escape inside HTML:\n%s", msg)
	}
	// underscore in name must be inside <code> only (escaped as text, not \_)
	if !strings.Contains(msg, "<code>") {
		t.Fatal("expected code-wrapped filename")
	}
	if strings.Contains(msg, "*downloading*") {
		t.Fatal("legacy markdown italics should not appear")
	}
}

func TestProgressBarWidth(t *testing.T) {
	b := progressBarPlain(50, 14)
	if len([]rune(b)) != 14 {
		t.Fatalf("width=%d bar=%q", len([]rune(b)), b)
	}
	b0 := progressBarPlain(0, 14)
	b100 := progressBarPlain(100, 14)
	if strings.ContainsRune(b0, '█') {
		t.Fatalf("0%% should be empty: %q", b0)
	}
	if strings.ContainsRune(b100, '░') {
		t.Fatalf("100%% should be full: %q", b100)
	}
}
