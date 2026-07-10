package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeRarPart2Only(t *testing.T) {
	vols := []rarVolumeCandidate{{path: "x.part2.rar", idx: 1}}
	warn, err := analyzeRarVolumeSet(vols)
	if err != nil {
		t.Fatal(err)
	}
	if warn == nil {
		t.Fatal("want warning")
	}
	if !strings.Contains(strings.Join(warn.Missing, ","), "part1") {
		t.Fatalf("missing should list part1: %v", warn.Missing)
	}
}

func TestJoinZipVolumesIncomplete(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "logs.part1.zip")
	p2 := filepath.Join(dir, "logs.part2.zip")
	if err := os.WriteFile(p1, []byte("AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("BBBB"), 0o644); err != nil {
		t.Fatal(err)
	}

	joined, warn, err := joinZipVolumes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if warn != nil {
		t.Fatalf("complete set should not warn: %v", warn)
	}
	data, _ := os.ReadFile(joined)
	if string(data) != "AAAABBBB" {
		t.Fatalf("joined = %q", data)
	}

	os.Remove(p1)
	joined2, warn2, err := joinZipVolumes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if warn2 == nil {
		t.Fatal("want partial warning for part2-only")
	}
	data2, _ := os.ReadFile(joined2)
	if string(data2) != "BBBB" {
		t.Fatalf("part2-only join = %q", data2)
	}
}

func TestAnyPartAccepted(t *testing.T) {
	// every common split name must be accepted as an upload
	names := []string{
		"logs.part1.rar",
		"logs.part2.rar",
		"logs.part99.rar",
		"logs.part02.rar",
		"logs.r00",
		"logs.r05",
		"logs.rar",
		"logs.zip",
		"logs.part2.zip",
		"logs.z01",
		"logs.z99",
		"logs.zip.001",
		"logs.zip.002",
		"logs.001",
		"logs.007",
		"dump.7z",
		"dump.7z.001",
		"dump.7z.003",
		"archive.rar.002",
		"vol3.rar",
		"volume_12.zip",
		"part2", // bare
		"my logs part 3.rar",
	}
	for _, n := range names {
		if !isArchiveUploadName(n) && !isPartFileAccepted(n) {
			t.Errorf("rejected: %q", n)
		}
	}
	// while collecting parts, even opaque names are fine
	if !isPartFileAccepted("document") {
		t.Error("opaque telegram name should be accepted as part")
	}
	if !isPartFileAccepted("file.bin") {
		t.Error("file.bin should be accepted as part in session")
	}
}

func TestRarVolumeIndexAnyPart(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		ok   bool
	}{
		{"a.part1.rar", 0, true},
		{"a.part2.rar", 1, true},
		{"a.part99.rar", 98, true},
		{"a.r00", 1, true},
		{"a.r05", 6, true},
		{"a.rar.003", 2, true},
		{"solo.rar", 0, true},
	}
	for _, c := range cases {
		idx, ok := rarVolumeIndex(c.name)
		if ok != c.ok || (ok && idx != c.idx) {
			t.Errorf("rarVolumeIndex(%q)=(%d,%v) want (%d,%v)", c.name, idx, ok, c.idx, c.ok)
		}
	}
}

func TestIsArchiveUploadNameMultipartZip(t *testing.T) {
	if !isArchiveUploadName("logs.part2.zip") {
		t.Fatal("part2.zip should be accepted")
	}
	if !isArchiveUploadName("logs.z01") {
		t.Fatal(".z01 should be accepted")
	}
	if !isArchiveUploadName("logs.zip.002") {
		t.Fatal(".zip.002 should be accepted")
	}
	if !shouldAwaitArchiveParts("logs.part2.rar") {
		t.Fatal("part2.rar should await /done")
	}
	if !shouldAwaitArchiveParts("logs.part1.zip") {
		t.Fatal("part1.zip should await /done")
	}
	if shouldAwaitArchiveParts("normal.zip") {
		t.Fatal("plain zip should auto-extract")
	}
}

func TestIsVolumeErr(t *testing.T) {
	if !isVolumeErr(ErrRarPartsMissing) {
		t.Fatal("ErrRarPartsMissing")
	}
	if isVolumeErr(nil) {
		t.Fatal("nil")
	}
}
