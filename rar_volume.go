package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	reRarPartNum = regexp.MustCompile(`(?i)(?:part|vol|volume)[._-]?(\d+)`)
	reRarDotNum  = regexp.MustCompile(`\.(\d+)\.rar$`)

	ErrRarPartsMissing = errors.New("rar multi-volume archive is missing earlier parts — send all .rar/.r00 parts to the bot, then /done")
)

const rarJoinBase = "__rarjoin"

func isRarContinuationExt(ext string) bool {
	if len(ext) != 4 || ext[0] != '.' || ext[1] != 'r' {
		return false
	}
	return ext[2] >= '0' && ext[2] <= '9' && ext[3] >= '0' && ext[3] <= '9'
}

func sanitizeArchiveFilename(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	base = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', 0:
			return -1
		default:
			return r
		}
	}, base)
	if base == "" || base == "." {
		return "archive"
	}
	return base
}

// rarVolumeIndex returns the 0-based volume index used by rardecode.
// Accepts any part number — part1, part2, part99, .r05, .rar.003, …
// Old-style naming: archive.rar → 0, archive.r00 → 1, archive.r01 → 2.
func rarVolumeIndex(name string) (int, bool) {
	low := strings.ToLower(filepath.Base(name))
	ext := filepath.Ext(low)

	// .r00 / .r01 …
	if isRarContinuationExt(ext) {
		n, err := strconv.Atoi(ext[2:])
		if err != nil {
			return 0, false
		}
		return n + 1, true
	}

	// archive.rar.001
	if m := regexp.MustCompile(`(?i)\.rar\.(\d+)$`).FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, true
		}
		if n <= 0 {
			return 0, true
		}
		return n - 1, true
	}

	// partN / volN anywhere in stem (works for .part2.rar, .part02.rar, …)
	if m := reRarPartNum.FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, true
		}
		if n <= 0 {
			return 0, true
		}
		return n - 1, true
	}

	// archive.001.rar
	if m := reRarDotNum.FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, true
		}
		if n <= 0 {
			return 0, true
		}
		return n - 1, true
	}

	// plain .rar → volume 0
	if ext == ".rar" {
		return 0, true
	}

	// raw .001/.002 splits that might be rar volumes
	if m := reNumericSplit.FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		if n <= 0 {
			return 0, true
		}
		return n - 1, true
	}

	return 0, false
}

func looksLikeMultipartRar(name string) bool {
	idx, ok := rarVolumeIndex(name)
	if !ok {
		return false
	}
	if idx > 0 {
		return true
	}
	low := strings.ToLower(filepath.Base(name))
	if reRarPartNum.MatchString(low) {
		return true
	}
	return isRarContinuationExt(filepath.Ext(low))
}

type rarVolumeCandidate struct {
	path string
	idx  int
}

func listRarVolumes(dir string) ([]rarVolumeCandidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []rarVolumeCandidate
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, rarJoinBase) || strings.HasPrefix(name, "__zipjoin") || strings.HasPrefix(name, "__7zout") {
			continue
		}
		// Prefer rar-family; also pick numeric splits when no clearer type.
		low := strings.ToLower(name)
		isRarFamily := strings.Contains(low, ".rar") || isRarContinuationExt(filepath.Ext(low)) ||
			(rePartInName.MatchString(low) && !strings.Contains(low, "zip") && !strings.Contains(low, ".7z"))
		if !isRarFamily && !reNumericSplit.MatchString(low) {
			// skip pure zip / unrelated files
			if strings.Contains(low, "zip") || strings.HasSuffix(low, ".7z") {
				continue
			}
			// still accept if rarVolumeIndex understands it
		}
		idx, ok := rarVolumeIndex(name)
		if !ok {
			continue
		}
		// Don't claim plain .zip as rar
		if strings.HasSuffix(low, ".zip") && !strings.Contains(low, "rar") {
			continue
		}
		out = append(out, rarVolumeCandidate{
			path: filepath.Join(dir, name),
			idx:  idx,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].idx != out[j].idx {
			return out[i].idx < out[j].idx
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func formatRarVolumeList(vols []rarVolumeCandidate) string {
	names := make([]string, len(vols))
	for i, v := range vols {
		names[i] = filepath.Base(v.path)
	}
	return strings.Join(names, "`, `")
}

// analyzeRarVolumeSet describes completeness without blocking extract.
// Incomplete sets return a PartialExtractWarning — callers still try best-effort.
func analyzeRarVolumeSet(vols []rarVolumeCandidate) (warn *PartialExtractWarning, err error) {
	if len(vols) == 0 {
		return nil, fmt.Errorf("no rar parts found — send .rar / .part1.rar / .r00 files, then /done")
	}
	seen := map[int]string{}
	minIdx, maxIdx := vols[0].idx, vols[0].idx
	have := make([]string, 0, len(vols))
	for _, v := range vols {
		if prev, ok := seen[v.idx]; ok {
			// keep first, note duplicate in warning later
			_ = prev
		} else {
			seen[v.idx] = v.path
		}
		have = append(have, filepath.Base(v.path))
		if v.idx < minIdx {
			minIdx = v.idx
		}
		if v.idx > maxIdx {
			maxIdx = v.idx
		}
	}
	var missing []string
	for i := 0; i < minIdx; i++ {
		missing = append(missing, fmt.Sprintf("part%d", i+1))
	}
	for i := minIdx; i <= maxIdx; i++ {
		if _, ok := seen[i]; !ok {
			missing = append(missing, fmt.Sprintf("part%d", i+1))
		}
	}
	if len(missing) > 0 || minIdx > 0 {
		note := "incomplete multi-volume rar — extracting whatever is readable"
		if minIdx > 0 {
			note = "missing earlier volume(s) — best-effort extract (may need 7z for mid-parts only)"
		}
		warn = &PartialExtractWarning{Have: have, Missing: missing, Note: note}
	}
	return warn, nil
}

// validateRarVolumeSet kept for callers/tests that want hard errors on total
// emptiness only. Incomplete sets no longer fail — use analyzeRarVolumeSet.
func validateRarVolumeSet(vols []rarVolumeCandidate) error {
	_, err := analyzeRarVolumeSet(vols)
	return err
}

func linkOrCopy(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func removeRarJoinArtifacts(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, rarJoinBase+".*"))
	for _, m := range matches {
		os.Remove(m)
	}
}

// isNewNamingSet reports whether the volume set uses RAR5 "partN.rar" naming
// (e.g. dump.part1.rar, dump.part02.rar). rardecode chains these natively via
// nextNewVolName, which relies on the "partNN" digit pattern in the filename —
// so they MUST be opened under their original names, never renamed.
//
// The legacy ".r00"/".r01" scheme has no digit pattern rardecode can increment
// from the base ".rar", so those still need the __rarjoin rename.
func isNewNamingSet(vols []rarVolumeCandidate) bool {
	if len(vols) == 0 {
		return false
	}
	for _, v := range vols {
		base := strings.ToLower(filepath.Base(v.path))
		if !reRarPartNum.MatchString(base) {
			return false
		}
	}
	return true
}

// prepareRarVolumesForDecode returns the path rardecode.OpenReader should open,
// plus an optional partial-set warning. Incomplete multi-volume sets are allowed
// — we open the lowest available volume and let the reader recover what it can.
//
// Two strategies, picked by the naming scheme:
//
//  1. New naming (partN.rar): native passthrough of the lowest available part.
//  2. Old naming (.r00/.r01): renumber available vols into __rarjoin.rNN so
//     rardecode's old-naming chain can walk them (gaps tolerated).
func prepareRarVolumesForDecode(dir string) (string, *PartialExtractWarning, error) {
	vols, err := listRarVolumes(dir)
	if err != nil {
		return "", nil, err
	}
	warn, err := analyzeRarVolumeSet(vols)
	if err != nil {
		return "", nil, err
	}
	if len(vols) == 1 {
		return vols[0].path, warn, nil
	}

	// Prefer the lowest-index volume as open target (ideally part1 / idx 0).
	// Sort is already by idx ascending from listRarVolumes.
	openVol := vols[0]

	// New-naming sets: native passthrough. rardecode finds part2.rar from
	// part1.rar on its own. Do NOT rename — that's the historical bug.
	// If part1 is missing we still open the lowest present part; rardecode
	// may return ErrBadVolumeNumber — caller falls back to 7z.
	if isNewNamingSet(vols) {
		removeRarJoinArtifacts(dir)
		return openVol.path, warn, nil
	}

	// Old-naming or mixed: map available volumes onto contiguous __rarjoin
	// names in ascending index order (best-effort when gaps exist).
	removeRarJoinArtifacts(dir)
	base := filepath.Join(dir, rarJoinBase)
	if err := linkOrCopy(vols[0].path, base+".rar"); err != nil {
		return "", warn, fmt.Errorf("prepare rar volume 1: %w", err)
	}
	for i := 1; i < len(vols); i++ {
		dst := fmt.Sprintf("%s.r%02d", base, i-1)
		if err := linkOrCopy(vols[i].path, dst); err != nil {
			return "", warn, fmt.Errorf("prepare rar volume %d: %w", i+1, err)
		}
	}
	return base + ".rar", warn, nil
}

func resolveRarOpenPath(dir string) (string, *PartialExtractWarning, error) {
	return prepareRarVolumesForDecode(dir)
}

// needsRarResolve reports whether rarPath still points at a raw multipart
// volume that needs prepareRarVolumesForDecode before rardecode can read it.
//
// Returns false when:
//   - the path is already a __rarjoin artifact (resolved), or
//   - the dir contains only this one .rar (single-volume, no join needed).
//
// This keeps processRarSpool from re-resolving (and re-creating join files)
// when the top-level caller already prepared the set, while still resolving
// nested RARs whose siblings haven't been processed yet.
func needsRarResolve(rarPath string) bool {
	base := strings.ToLower(filepath.Base(rarPath))
	if strings.HasPrefix(base, strings.ToLower(rarJoinBase)) {
		return false
	}
	dir := filepath.Dir(rarPath)
	vols, err := listRarVolumes(dir)
	if err != nil {
		return false
	}
	return len(vols) > 1
}

func removeArchiveFiles(dir string) {
	removeRarJoinArtifacts(dir)
	_ = os.Remove(filepath.Join(dir, "__zipjoin.zip"))
	_ = os.RemoveAll(filepath.Join(dir, "__7zout"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if isArchiveUploadName(ent.Name()) || strings.HasPrefix(ent.Name(), "__zipjoin") {
			os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
}

// shouldAwaitRarParts is deprecated alias — use shouldAwaitArchiveParts.
func shouldAwaitRarParts(name string) bool {
	return shouldAwaitArchiveParts(name)
}
