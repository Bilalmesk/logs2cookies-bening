package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNeedsRarResolve(t *testing.T) {
	t.Run("single rar does not need resolve", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "solo.rar")
		if needsRarResolve(filepath.Join(dir, "solo.rar")) {
			t.Error("single .rar should not need resolve")
		}
	})

	t.Run("new-naming multipart needs resolve", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "d.part1.rar")
		writeFile(t, dir, "d.part2.rar")
		if !needsRarResolve(filepath.Join(dir, "d.part1.rar")) {
			t.Error("raw multipart should need resolve")
		}
	})

	t.Run("old-naming multipart needs resolve", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "d.rar")
		writeFile(t, dir, "d.r00")
		if !needsRarResolve(filepath.Join(dir, "d.rar")) {
			t.Error("raw old-naming multipart should need resolve")
		}
	})

	t.Run("already-joined artifact does not need resolve", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "__rarjoin.rar")
		writeFile(t, dir, "__rarjoin.r00")
		if needsRarResolve(filepath.Join(dir, "__rarjoin.rar")) {
			t.Error("already-resolved __rarjoin path should not need re-resolve")
		}
	})
}

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("Rar!\x1a\x07\x01\x00xxxx"), 0o644); err != nil {
		t.Fatal(err)
	}
}
