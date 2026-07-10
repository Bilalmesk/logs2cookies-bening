package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeVol writes a placeholder volume file into dir.
func writeVol(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("Rar!\x1a\x07\x01\x00"+strings.Repeat("x", 32)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareRarVolumes_NewNamingPreservesNames is the regression test for the
// RAR5 multipart bug. New-naming volumes (part1.rar / part2.rar / ...) MUST
// resolve to the first part's ORIGINAL name — rardecode chains them natively
// via nextNewVolName, which relies on the "partNN" digit pattern.
//
// The old code renamed every set to __rarjoin.rar + __rarjoin.r00, which
// destroys the digit pattern and breaks rardecode's volume chaining (it
// panics or silently fails to find part 2).
func TestPrepareRarVolumes_NewNamingPreservesNames(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
	}{
		{
			name:  "simple partN",
			parts: []string{"dump.part1.rar", "dump.part2.rar", "dump.part3.rar"},
		},
		{
			name:  "zero-padded partNN",
			parts: []string{"dump.part01.rar", "dump.part02.rar", "dump.part03.rar"},
		},
		{
			name:  "spaced name partN",
			parts: []string{"@ft7 logs 17540.part1.rar", "@ft7 logs 17540.part2.rar", "@ft7 logs 17540.part3.rar"},
		},
		{
			name:  "two parts only",
			parts: []string{"log.part1.rar", "log.part2.rar"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, p := range c.parts {
				writeVol(t, dir, p)
			}
			got, _, err := prepareRarVolumesForDecode(dir)
			if err != nil {
				t.Fatalf("prepareRarVolumesForDecode: %v", err)
			}
			base := filepath.Base(got)
			// Must be the first part's original name (native chaining).
			if base != c.parts[0] {
				t.Errorf("open path:\n  got  %q\n  want %q (first part, native chaining)", base, c.parts[0])
			}
			// Must NOT have created any __rarjoin artifacts.
			if entries, _ := os.ReadDir(dir); entries != nil {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), rarJoinBase) {
						t.Errorf("new-naming set should not create join artifacts, found: %s", e.Name())
					}
				}
			}
		})
	}
}

// TestPrepareRarVolumes_OldNamingStillJoins: old-style .r00/.r01 volumes have
// no "part" keyword, so rardecode's new-naming can't chain them. The join
// rename is still the correct strategy here.
func TestPrepareRarVolumes_OldNamingStillJoins(t *testing.T) {
	dir := t.TempDir()
	writeVol(t, dir, "dump.rar")
	writeVol(t, dir, "dump.r00")
	writeVol(t, dir, "dump.r01")

	got, _, err := prepareRarVolumesForDecode(dir)
	if err != nil {
		t.Fatalf("prepareRarVolumesForDecode: %v", err)
	}
	base := filepath.Base(got)
	if !strings.HasPrefix(base, rarJoinBase) {
		t.Errorf("old-naming set should use join artifacts, got open path %q", base)
	}
	if _, err := os.Stat(filepath.Join(dir, rarJoinBase+".r00")); err != nil {
		t.Errorf("missing join r00: %v", err)
	}
}

// TestPrepareRarVolumes_SinglePartPassthrough: a lone .rar (no siblings) is
// not a multi-volume set and must resolve to itself untouched.
func TestPrepareRarVolumes_SinglePartPassthrough(t *testing.T) {
	dir := t.TempDir()
	writeVol(t, dir, "solo.rar")

	got, _, err := prepareRarVolumesForDecode(dir)
	if err != nil {
		t.Fatalf("prepareRarVolumesForDecode: %v", err)
	}
	if filepath.Base(got) != "solo.rar" {
		t.Errorf("single .rar should pass through, got %q", got)
	}
}

// TestPrepareRarVolumes_NewNamingKeepsAllPartsReachable: after resolution, all
// original part files must still exist in the dir (no file got deleted/corrupted).
func TestPrepareRarVolumes_NewNamingKeepsAllPartsReachable(t *testing.T) {
	dir := t.TempDir()
	parts := []string{"d.part1.rar", "d.part2.rar", "d.part3.rar"}
	for _, p := range parts {
		writeVol(t, dir, p)
	}
	if _, _, err := prepareRarVolumesForDecode(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("part %q missing after resolution: %v", p, err)
		}
	}
}
