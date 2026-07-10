package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwaples/rardecode/v2"
	zipx "github.com/yeka/zip"
)

// processArchive is the legacy in-memory wrapper kept for tests/CLI.
// Production uses processArchiveSpool directly to avoid materializing
// every cookie in RAM.
func processArchive(archivePath, filter, password string) ([]CookieRow, Stats, error) {
	tmpDir, err := os.MkdirTemp("", "spool-*")
	if err != nil {
		return nil, Stats{}, err
	}
	defer os.RemoveAll(tmpDir)
	spool, err := NewSpool(filepath.Join(tmpDir, "s"))
	if err != nil {
		return nil, Stats{}, err
	}
	perr := processArchiveSpool(archivePath, filter, password, 0, spool)
	spool.Close()
	stats := spool.Stats()
	if perr != nil {
		return nil, stats, perr
	}
	rows, rerr := readSpool(spool.Path())
	return rows, stats, rerr
}

var ErrPasswordRequired = errors.New("password required")
var ErrBadPassword = errors.New("bad password")

const MAX_NEST_DEPTH = 4

// processArchiveSpool is the streaming entry point — every cookie row is
// pushed to the spool (disk) instead of accumulated in a slice.
func processArchiveSpool(archivePath, filter, password string, depth int, spool *Spool) error {
	low := strings.ToLower(archivePath)
	if strings.HasSuffix(low, ".rar") {
		return processRarSpool(archivePath, filter, password, depth, spool)
	}
	return processZipSpool(archivePath, filter, password, depth, spool)
}

func isNestedArchive(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".rar")
}

func spawnNestedSpool(reader io.Reader, name string, filter, password string, depth int, spool *Spool) error {
	if depth+1 >= MAX_NEST_DEPTH {
		return nil
	}
	tmp, err := os.CreateTemp("", "nest-*"+filepath.Ext(name))
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, io.LimitReader(reader, MAX_NEST_STREAM_BYTES)); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return processArchiveSpool(tmpPath, filter, password, depth+1, spool)
}

func processZipSpool(zipPath, filter, password string, depth int, spool *Spool) error {
	zr, err := zipx.OpenReader(zipPath)
	if err != nil {
		// Corrupt/incomplete multi-part zip → 7z best-effort
		if ferr := try7zBestEffort(zipPath, filter, spool); ferr == nil {
			spool.SetPartial(&PartialExtractWarning{
				Note: "zip open failed in pure-Go; recovered via 7z best-effort",
			})
			return nil
		}
		return err
	}
	defer zr.Close()

	// Only require password if cookie files themselves are encrypted.
	// Non-cookie encrypted entries (Passwords.txt, SystemInfo.txt, etc.)
	// are skipped silently — don't bail on the whole archive for them.
	if password == "" {
		for _, zf := range zr.File {
			if !zf.FileInfo().IsDir() && zf.IsEncrypted() && looksLikeCookieFile(zf.Name) {
				return ErrPasswordRequired
			}
		}
	}

	flow := strings.ToLower(filter)
	pwFails := 0
	gotCookieData := false

	for _, zf := range zr.File {
		spool.OnEntry()
		if zf.FileInfo().IsDir() {
			continue
		}
		name := zf.Name

		if isNestedArchive(name) && depth+1 < MAX_NEST_DEPTH {
			if zf.IsEncrypted() {
				if password == "" {
					continue // skip encrypted nested archives we can't open
				}
				zf.SetPassword(password)
			}
			rc, oerr := zf.Open()
			if oerr != nil {
				if password != "" && isPasswordErr(oerr) {
					pwFails++
				}
				continue
			}
			_ = spawnNestedSpool(rc, name, filter, password, depth, spool)
			rc.Close()
			continue
		}

		if int64(zf.UncompressedSize64) > MAX_FILE_BYTES {
			continue
		}
		if !looksLikeCookieFile(name) {
			continue
		}

		if zf.IsEncrypted() {
			if password == "" {
				continue // already asked above — skip remaining encrypted cookie files
			}
			zf.SetPassword(password)
		}

		f, err := zf.Open()
		if err != nil {
			if isPasswordErr(err) {
				if password != "" {
					pwFails++
				}
				continue // try other files; unencrypted cookies may still work
			}
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(f, MAX_FILE_BYTES))
		f.Close()
		if rerr != nil {
			if isPasswordErr(rerr) {
				if password != "" {
					pwFails++
				}
				continue
			}
			continue
		}

		parsed := parseCookieFile(name, data)
		data = nil
		if len(parsed) == 0 {
			continue
		}
		gotCookieData = true
		spool.OnCookieFile(name)

		for _, r := range parsed {
			if flow != "" && !domainFilterMatch(r.Domain, flow) {
				continue
			}
			spool.Add(r)
		}
	}
	// Only report wrong password if we tried one, hits failed decrypt, and got nothing usable.
	if password != "" && pwFails > 0 && !gotCookieData && !spoolHasCookies(spool) {
		return ErrBadPassword
	}
	return nil
}

// domainFilterMatch: if filter looks like a hostname (contains a dot), use
// subdomain-aware matching; otherwise keep loose substring (e.g. "steam").
func domainFilterMatch(cookieDomain, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	if strings.Contains(filter, ".") {
		return domainMatchesTarget(cookieDomain, filter)
	}
	return domainContainsLoose(cookieDomain, filter)
}

func processRarSpool(rarPath, filter, password string, depth int, spool *Spool) error {
	// Caller (runArchiveExtraction) has already resolved multi-volume sets to
	// a single openable path. We only re-resolve for nested RARs (spawned via
	// spawnNestedSpool) whose sibling volumes live in the same temp dir and
	// haven't been prepared yet.
	openPath := rarPath
	var partial *PartialExtractWarning
	if needsRarResolve(rarPath) {
		resolved, warn, err := resolveRarOpenPath(filepath.Dir(rarPath))
		if err == nil {
			openPath = resolved
			partial = warn
		}
		// incomplete sets no longer hard-fail here — try openPath as-is
	}

	opts := []rardecode.Option{}
	if password != "" {
		opts = append(opts, rardecode.Password(password))
	}
	r, err := rardecode.OpenReader(openPath, opts...)
	if err != nil {
		if errors.Is(err, rardecode.ErrBadPassword) || isPasswordErr(err) {
			if password == "" {
				return ErrPasswordRequired
			}
			return ErrBadPassword
		}
		// Mid-volume only / broken chain → try 7z best-effort, then soft-fail.
		if isVolumeErr(err) {
			if ferr := try7zBestEffort(openPath, filter, spool); ferr == nil {
				spool.SetPartial(mergePartial(partial, &PartialExtractWarning{
					Note: "opened mid-volume set via 7z best-effort",
				}))
				return nil
			}
			// Last resort: try every sibling volume path with 7z
			if ferr := try7zBestEffort(rarPath, filter, spool); ferr == nil {
				spool.SetPartial(mergePartial(partial, &PartialExtractWarning{
					Note: "opened via 7z best-effort after rardecode refused the set",
				}))
				return nil
			}
			// If we already have cookies from a parent nested context, don't wipe them.
			if spoolHasCookies(spool) {
				spool.SetPartial(mergePartial(partial, &PartialExtractWarning{
					Note: "volume open failed mid-set; kept cookies found so far",
				}))
				return nil
			}
			return fmt.Errorf("%w: %v — send remaining parts or install 7z for mid-part recovery", ErrRarPartsMissing, err)
		}
		return err
	}
	defer r.Close()

	flow := strings.ToLower(filter)
	gotAny := false

	for {
		hdr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, rardecode.ErrBadPassword) || isPasswordErr(err) {
				if password == "" {
					return ErrPasswordRequired
				}
				return ErrBadPassword
			}
			// Missing continuation volume / bad volume mid-stream:
			// stop cleanly if we already pulled cookies (partial success).
			if isVolumeErr(err) {
				if gotAny || spoolHasCookies(spool) {
					spool.SetPartial(mergePartial(partial, &PartialExtractWarning{
						Note: "hit missing volume mid-archive — extracted files before the gap",
					}))
					return nil
				}
				// Nothing yet — try 7z on the open path
				if ferr := try7zBestEffort(openPath, filter, spool); ferr == nil {
					spool.SetPartial(mergePartial(partial, &PartialExtractWarning{
						Note: "rardecode stalled on volumes; recovered via 7z",
					}))
					return nil
				}
				return fmt.Errorf("%w: %v", ErrRarPartsMissing, err)
			}
			// Other read errors: soft-continue if we have cookies
			if gotAny || spoolHasCookies(spool) {
				spool.SetPartial(mergePartial(partial, &PartialExtractWarning{
					Note: "archive read error mid-stream — partial results kept",
				}))
				return nil
			}
			return err
		}
		spool.OnEntry()
		if hdr.IsDir {
			continue
		}
		name := hdr.Name

		if isNestedArchive(name) && depth+1 < MAX_NEST_DEPTH {
			if (hdr.Encrypted || hdr.HeaderEncrypted) && password == "" {
				continue
			}
			_ = spawnNestedSpool(r, name, filter, password, depth, spool)
			continue
		}

		if (hdr.Encrypted || hdr.HeaderEncrypted) && password == "" {
			if looksLikeCookieFile(name) {
				return ErrPasswordRequired
			}
			continue
		}

		if hdr.UnPackedSize > MAX_FILE_BYTES {
			continue
		}
		if !looksLikeCookieFile(name) {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(r, MAX_FILE_BYTES))
		if err != nil {
			if errors.Is(err, rardecode.ErrBadPassword) {
				if password == "" {
					return ErrPasswordRequired
				}
				return ErrBadPassword
			}
			// truncated file across missing volume — skip this entry
			continue
		}

		parsed := parseCookieFile(name, data)
		data = nil
		if len(parsed) == 0 {
			continue
		}
		spool.OnCookieFile(name)
		gotAny = true

		for _, cr := range parsed {
			if flow != "" && !domainFilterMatch(cr.Domain, flow) {
				continue
			}
			spool.Add(cr)
		}
	}
	if partial != nil {
		spool.SetPartial(partial)
	}
	return nil
}

func isVolumeErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, rardecode.ErrBadVolumeNumber) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrRarPartsMissing) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "volume") ||
		strings.Contains(s, "bad volume") ||
		strings.Contains(s, "unexpected eof") ||
		strings.Contains(s, "no such file")
}

func spoolHasCookies(spool *Spool) bool {
	if spool == nil {
		return false
	}
	st := spool.Stats()
	return st.UniqueCookies > 0 || st.CookieFiles > 0
}

func mergePartial(a, b *PartialExtractWarning) *PartialExtractWarning {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := *a
	if b.Note != "" {
		if out.Note != "" {
			out.Note = out.Note + "; " + b.Note
		} else {
			out.Note = b.Note
		}
	}
	out.Missing = append(append([]string{}, a.Missing...), b.Missing...)
	out.Have = append(append([]string{}, a.Have...), b.Have...)
	return &out
}

func isPasswordErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "password") || strings.Contains(s, "decryption") || strings.Contains(s, "encrypted")
}
