package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRarVolumeIndex(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		ok   bool
	}{
		{"logs.rar", 0, true},
		{"logs.part1.rar", 0, true},
		{"logs.part02.rar", 1, true},
		{"logs.part3.rar", 2, true},
		{"logs.r00", 1, true},
		{"logs.r01", 2, true},
		{"archive.001.rar", 0, true},
		{"readme.txt", 0, false},
	}
	for _, c := range cases {
		idx, ok := rarVolumeIndex(c.name)
		if ok != c.ok || (ok && idx != c.idx) {
			t.Errorf("rarVolumeIndex(%q) = (%d, %v), want (%d, %v)", c.name, idx, ok, c.idx, c.ok)
		}
	}
}

func TestResolveRarOpenPath(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// part2-only is allowed now (best-effort) — returns a warning, not hard error
	write("logs.part2.rar")
	got, warn, err := resolveRarOpenPath(dir)
	if err != nil {
		t.Fatalf("part2-only should resolve best-effort, got err %v", err)
	}
	if filepath.Base(got) != "logs.part2.rar" {
		t.Fatalf("open path = %q, want logs.part2.rar", got)
	}
	if warn == nil || len(warn.Missing) == 0 {
		t.Fatalf("expected partial warning for missing part1, warn=%v", warn)
	}

	write("logs.part1.rar")
	got, warn, err = resolveRarOpenPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if warn != nil {
		t.Fatalf("complete set should not warn, got %v", warn)
	}
	if !strings.HasSuffix(got, "__rarjoin.rar") && filepath.Base(got) != "logs.part1.rar" {
		t.Fatalf("open path = %q", got)
	}
}

func TestValidateRarVolumeSetMissingMiddle(t *testing.T) {
	vols := []rarVolumeCandidate{
		{path: "a.part1.rar", idx: 0},
		{path: "a.part3.rar", idx: 2},
	}
	// incomplete is a warning now, not a hard error
	warn, err := analyzeRarVolumeSet(vols)
	if err != nil {
		t.Fatalf("analyze should not hard-fail incomplete sets: %v", err)
	}
	if warn == nil {
		t.Fatal("want partial warning for gap")
	}
	joined := strings.Join(warn.Missing, ",")
	if !strings.Contains(joined, "part2") {
		t.Fatalf("expected part2 in missing: %v", warn.Missing)
	}
}

func TestPrepareRarVolumesSpacedName(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// New-naming (partN.rar) with spaces: must resolve to the first part's
	// original name — rardecode chains natively and the space is fine.
	write("@ft7logs premium 17540.part1.rar")
	write("@ft7logs premium 17540.part2.rar")
	write("@ft7logs premium 17540.part3.rar")
	got, _, err := prepareRarVolumesForDecode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "@ft7logs premium 17540.part1.rar" {
		t.Fatalf("spaced new-naming should pass through first part, got %q", got)
	}
}

func TestLooksLikeMultipartRar(t *testing.T) {
	if !looksLikeMultipartRar("dump.part1.rar") {
		t.Fatal("part1 should look multipart")
	}
	if looksLikeMultipartRar("dump.rar") {
		t.Fatal("plain rar should not look multipart")
	}
	if !looksLikeMultipartRar("dump.r00") {
		t.Fatal(".r00 should look multipart")
	}
}
