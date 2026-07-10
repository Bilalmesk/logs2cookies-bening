package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Multi-part naming we accept (any index — part1, part2, part99, mid-set only…):
//
//	name.partN.zip / .rar / .7z
//	name.zip.001 / name.rar.002 / name.7z.001
//	name.z01 / name.r00
//	name.001 / name.002  (raw numeric split)
//	anything with part|vol|volume + digits in the stem
var (
	reZipPartNum   = regexp.MustCompile(`(?i)(?:part|vol|volume)[._-]?(\d+)\.zip$`)
	reZipDotNum    = regexp.MustCompile(`(?i)\.zip\.(\d+)$`)
	reZipZExt      = regexp.MustCompile(`(?i)\.z(\d+)$`)
	reNumericSplit = regexp.MustCompile(`(?i)\.(\d{3,})$`) // .001 .002 .999
	rePartInName   = regexp.MustCompile(`(?i)(?:^|[._\-\s])(?:part|vol|volume)[._\-]?(\d+)`)
)

// PartialExtractWarning is attached when we recovered cookies from an incomplete
// multi-volume set (missing parts). Surface it to the user, don't fail the job.
type PartialExtractWarning struct {
	Have    []string
	Missing []string
	Note    string
}

func (w *PartialExtractWarning) String() string {
	if w == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("⚠️ *partial extract*")
	if w.Note != "" {
		b.WriteString(" — ")
		b.WriteString(w.Note)
	}
	if len(w.Have) > 0 {
		b.WriteString("\nhave: `")
		b.WriteString(strings.Join(w.Have, "`, `"))
		b.WriteString("`")
	}
	if len(w.Missing) > 0 {
		b.WriteString("\nmissing: `")
		b.WriteString(strings.Join(w.Missing, "`, `"))
		b.WriteString("`")
	}
	b.WriteString("\nsome files may be absent — cookies below are from what was readable")
	return b.String()
}

// --- zip multi-part ---

func isZipMultipartName(name string) bool {
	low := strings.ToLower(filepath.Base(name))
	if reZipPartNum.MatchString(low) || reZipDotNum.MatchString(low) || reZipZExt.MatchString(low) {
		return true
	}
	// raw numeric split next to a zip base: logs.001 with zip intent
	if reNumericSplit.MatchString(low) && (strings.Contains(low, "zip") || strings.Contains(low, "part") || strings.Contains(low, "vol")) {
		return true
	}
	return false
}

// isArchiveUploadName — accept every common archive + split-volume name.
// Any part index is fine (part2 alone, .r05, .007, mid-set only…).
func isArchiveUploadName(name string) bool {
	low := strings.ToLower(filepath.Base(name))
	if low == "" || low == "." {
		return false
	}
	// full archives
	if strings.HasSuffix(low, ".zip") || strings.HasSuffix(low, ".rar") || strings.HasSuffix(low, ".7z") {
		return true
	}
	// classic rar continuations .r00–.r99
	if isRarContinuationExt(filepath.Ext(low)) {
		return true
	}
	// zip continuations .z01–.z99, .zip.001
	if reZipZExt.MatchString(low) || reZipDotNum.MatchString(low) {
		return true
	}
	// 7z multi-vol: archive.7z.001
	if strings.Contains(low, ".7z.") && reNumericSplit.MatchString(low) {
		return true
	}
	// rar multi-vol: archive.rar.001
	if strings.Contains(low, ".rar.") && reNumericSplit.MatchString(low) {
		return true
	}
	// raw numeric split: dump.001 / dump.002
	if reNumericSplit.MatchString(low) {
		return true
	}
	// anything with part/vol/volume + number (part2.bin, vol03, …)
	if rePartInName.MatchString(low) {
		return true
	}
	return false
}

// looksLikeSplitVolume — true when this filename is almost certainly one
// piece of a multi-volume set (any index, including part1 or part99).
func looksLikeSplitVolume(name string) bool {
	low := strings.ToLower(filepath.Base(name))
	if isRarContinuationExt(filepath.Ext(low)) || reZipZExt.MatchString(low) {
		return true
	}
	if reZipDotNum.MatchString(low) || rePartInName.MatchString(low) {
		return true
	}
	if reNumericSplit.MatchString(low) {
		return true
	}
	if looksLikeMultipartRar(name) || isZipMultipartName(name) {
		return true
	}
	// name.partN.rar already covered by looksLikeMultipartRar
	return false
}

func shouldAwaitArchiveParts(name string) bool {
	// Any split-looking volume waits for more parts / /done.
	// Plain single .zip auto-extracts; plain solo .rar still waits (may be
	// volume 1 of old-style set with .r00 siblings arriving next).
	low := strings.ToLower(filepath.Base(name))
	if looksLikeSplitVolume(name) {
		return true
	}
	if strings.HasSuffix(low, ".rar") || strings.HasSuffix(low, ".7z") {
		return true
	}
	return false
}

// isPartFileAccepted is used while a multi-part session is open: accept ANY
// document the user sends as another volume (Telegram renames, no extension, …).
func isPartFileAccepted(name string) bool {
	if isArchiveUploadName(name) {
		return true
	}
	// empty / weird telegram names still accepted as opaque parts
	base := strings.TrimSpace(filepath.Base(name))
	return base != "" && base != "." && base != ".."
}

// zipVolumeIndex returns 0-based part index for multi-part zip names.
func zipVolumeIndex(name string) (int, bool) {
	low := strings.ToLower(filepath.Base(name))
	if m := reZipPartNum.FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		if n <= 0 {
			return 0, true
		}
		return n - 1, true
	}
	if m := reZipDotNum.FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		if n <= 0 {
			return 0, true
		}
		return n - 1, true
	}
	if m := reZipZExt.FindStringSubmatch(low); len(m) == 2 {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		// .z01 is volume 1 (after base .zip which is 0)
		return n, true
	}
	// plain .zip can be volume 0 of a .z01 set
	if strings.HasSuffix(low, ".zip") && !reZipPartNum.MatchString(low) && !reZipDotNum.MatchString(low) {
		return 0, true
	}
	return 0, false
}

type zipVolumeCandidate struct {
	path string
	idx  int
}

func listZipVolumes(dir string) ([]zipVolumeCandidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []zipVolumeCandidate
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, rarJoinBase) || strings.HasPrefix(name, "__zipjoin") {
			continue
		}
		// only consider zip-family names
		low := strings.ToLower(name)
		if !strings.Contains(low, "zip") && !reZipZExt.MatchString(low) {
			continue
		}
		idx, ok := zipVolumeIndex(name)
		if !ok {
			continue
		}
		// plain .zip alone without multipart siblings is a single archive —
		// still list it; join logic handles len==1.
		out = append(out, zipVolumeCandidate{
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

// joinZipVolumes concatenates multi-part zip segments in index order into
// __zipjoin.zip. Works for raw-split zips (HJSplit / partN.zip byte splits).
// Incomplete sets: joins whatever consecutive-from-lowest we have (best effort).
func joinZipVolumes(dir string) (joined string, warn *PartialExtractWarning, err error) {
	vols, err := listZipVolumes(dir)
	if err != nil {
		return "", nil, err
	}
	// Filter to multipart-looking sets only (need ≥1 partN or .z0N)
	var multi []zipVolumeCandidate
	for _, v := range vols {
		base := filepath.Base(v.path)
		if isZipMultipartName(base) || reZipZExt.MatchString(strings.ToLower(base)) {
			multi = append(multi, v)
		}
	}
	// Include plain .zip if .z01 siblings exist
	if len(multi) > 0 {
		for _, v := range vols {
			base := filepath.Base(v.path)
			if strings.HasSuffix(strings.ToLower(base), ".zip") && !isZipMultipartName(base) {
				// check not already in multi
				found := false
				for _, m := range multi {
					if m.path == v.path {
						found = true
						break
					}
				}
				if !found {
					multi = append(multi, v)
				}
			}
		}
		sort.Slice(multi, func(i, j int) bool { return multi[i].idx < multi[j].idx })
		vols = multi
	}

	if len(vols) == 0 {
		return "", nil, fmt.Errorf("no zip parts found")
	}
	if len(vols) == 1 && !isZipMultipartName(filepath.Base(vols[0].path)) {
		// single normal zip
		return vols[0].path, nil, nil
	}

	have := make([]string, len(vols))
	seen := map[int]string{}
	minIdx, maxIdx := vols[0].idx, vols[0].idx
	for i, v := range vols {
		have[i] = filepath.Base(v.path)
		seen[v.idx] = v.path
		if v.idx < minIdx {
			minIdx = v.idx
		}
		if v.idx > maxIdx {
			maxIdx = v.idx
		}
	}
	var missing []string
	for i := minIdx; i <= maxIdx; i++ {
		if _, ok := seen[i]; !ok {
			missing = append(missing, fmt.Sprintf("part%d", i+1))
		}
	}
	if minIdx > 0 {
		for i := 0; i < minIdx; i++ {
			missing = append([]string{fmt.Sprintf("part%d", i+1)}, missing...)
		}
	}

	// Join in sorted index order (gaps: skip missing — best effort raw concat)
	outPath := filepath.Join(dir, "__zipjoin.zip")
	_ = os.Remove(outPath)
	out, err := os.Create(outPath)
	if err != nil {
		return "", nil, err
	}
	defer out.Close()

	// stable order by idx
	order := make([]zipVolumeCandidate, len(vols))
	copy(order, vols)
	sort.Slice(order, func(i, j int) bool { return order[i].idx < order[j].idx })

	for _, v := range order {
		f, err := os.Open(v.path)
		if err != nil {
			out.Close()
			os.Remove(outPath)
			return "", nil, err
		}
		_, copyErr := io.Copy(out, f)
		f.Close()
		if copyErr != nil {
			out.Close()
			os.Remove(outPath)
			return "", nil, copyErr
		}
	}

	if len(missing) > 0 || minIdx > 0 {
		warn = &PartialExtractWarning{
			Have:    have,
			Missing: missing,
			Note:    "incomplete multi-part zip — joined available segments",
		}
	}
	return outPath, warn, nil
}

// --- 7z fallback ---

func find7z() string {
	for _, c := range []string{"7zz", "7z", "7za", "7zr"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	// common absolute drops
	for _, c := range []string{"/tmp/7zz", "/usr/local/bin/7zz", "/usr/bin/7z"} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// extractWith7z unpacks archive into destDir using 7-Zip (handles incomplete
// multi-volume RAR/ZIP better than pure-Go libraries). Returns nil on success.
func extractWith7z(archive, destDir string) error {
	bin := find7z()
	if bin == "" {
		return fmt.Errorf("7z not available")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	// -y assume yes, -o output dir (no space after -o for 7z)
	// -spe don't eliminate root folder duplication
	// For multi-vol, point at first/any part; 7z finds siblings in same dir.
	cmd := exec.Command(bin, "x", "-y", "-bd", "-o"+destDir, archive)
	cmd.Dir = filepath.Dir(archive)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z: %v: %s", err, truncate(string(out), 300))
	}
	return nil
}

// processDirSpool walks an extracted directory (e.g. from 7z) for cookie files.
func processDirSpool(root, filter string, spool *Spool) error {
	flow := strings.ToLower(filter)
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		spool.OnEntry()
		rel, _ := filepath.Rel(root, path)
		if !looksLikeCookieFile(rel) && !looksLikeCookieFile(path) {
			return nil
		}
		if info.Size() > MAX_FILE_BYTES {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		parsed := parseCookieFile(rel, data)
		if len(parsed) == 0 {
			return nil
		}
		spool.OnCookieFile(rel)
		for _, r := range parsed {
			if flow != "" && !domainFilterMatch(r.Domain, flow) {
				continue
			}
			spool.Add(r)
		}
		return nil
	})
}

// try7zBestEffort extracts archive via 7z into a temp dir under jobDir and
// feeds cookie files into the spool. Used when pure-Go RAR/ZIP open fails on
// incomplete multi-volume sets. Tries the given path first, then every sibling
// part in the same directory (so part2-only still gets a shot).
func try7zBestEffort(archivePath, filter string, spool *Spool) error {
	if find7z() == "" {
		return fmt.Errorf("no 7z binary for multi-volume fallback")
	}
	dir := filepath.Dir(archivePath)
	dest := filepath.Join(dir, "__7zout")

	candidates := []string{archivePath}
	// every sibling part — any index
	for _, p := range listDirParts(dir) {
		if p == archivePath {
			continue
		}
		candidates = append(candidates, p)
	}

	var lastErr error
	for _, c := range candidates {
		_ = os.RemoveAll(dest)
		if err := extractWith7z(c, dest); err != nil {
			lastErr = err
			continue
		}
		if err := processDirSpool(dest, filter, spool); err != nil {
			lastErr = err
			continue
		}
		// success if we got anything OR extract produced files
		if spoolHasCookies(spool) {
			_ = os.RemoveAll(dest)
			return nil
		}
		// 7z may have extracted non-cookie files; still count as success if tree non-empty
		if entries, _ := os.ReadDir(dest); len(entries) > 0 {
			_ = os.RemoveAll(dest)
			return nil
		}
	}
	_ = os.RemoveAll(dest)
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("7z produced no output")
}

func listDirParts(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, rarJoinBase) || strings.HasPrefix(name, "__zipjoin") ||
			strings.HasPrefix(name, "__7zout") || strings.HasSuffix(name, ".spool") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}
