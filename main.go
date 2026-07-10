package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amarnathcjd/gogram/telegram"
)

const (
	MAX_ARCHIVE_BYTES_URL = 5 * 1024 * 1024 * 1024
	MAX_FILE_BYTES        = 50 * 1024 * 1024
	WORK_TTL              = 30 * time.Minute
)

var COOKIE_NAME_HINTS = []string{
	"cookie", "cookies", "netscape_cookies",
}

// browserNames are used to catch cookie files named after the browser
// (e.g. chrome.txt, firefox.json) that don't have "cookie" in the path.
var browserNames = []string{
	"chrome", "chromium", "firefox", "edge", "msedge", "opera",
	"brave", "yandex", "vivaldi", "safari", "gecko",
}

type CookieRow struct {
	Domain     string
	Flag       string
	Path       string
	Secure     string
	Expiration string
	Name       string
	Value      string
	Source     string
}

type Stats struct {
	ArchiveFiles  int
	CookieFiles   int
	TotalCookies  int
	UniqueCookies int
	DomainCounts  map[string]int
	BrowserHints  map[string]int
	// Partial is set when multi-volume extract ran with missing parts.
	Partial *PartialExtractWarning
}

func workRoot() string {
	if r := strings.TrimSpace(os.Getenv("WORK_ROOT")); r != "" {
		return r
	}
	return "work"
}

func main() {
	// Soft-cap heap at 7 GiB so the GC reclaims aggressively before the
	// 8 GiB cgroup OOM-kills us. Cheap insurance.
	debug.SetMemoryLimit(7 << 30)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "download":
			runCLIDownload(os.Args[2:])
			return
		case "extract":
			runCLIExtract(os.Args[2:])
			return
		case "presets", "list-presets":
			runCLIExtract([]string{"--list-presets"})
			return
		}
	}
	root := workRoot()
	if err := runTelegramBot(root); err != nil {
		log.Fatalf("bot: %v", err)
	}
}

func handle(bot *Bot, m *telegram.NewMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
			reply(bot, m, fmt.Sprintf("internal error: %v", r))
		}
	}()

	if m.IsCommand() {
		switch commandName(m) {
		case "start", "help":
			reply(bot, m, helpText())
		case "cancel":
			if s := getActiveSessionByChat(m.ChatID()); s != nil {
				cleanupSession(s)
				reply(bot, m, "session cancelled, files cleaned up")
			} else {
				reply(bot, m, "no active session")
			}
		case "done":
			if s := getActiveSessionByChat(m.ChatID()); s != nil && s.State == StateAwaitingParts {
				finishMultipartUpload(bot, m, s)
			} else {
				reply(bot, m, "no multi-part upload in progress — send archive parts first")
			}
		case "preset":
			handlePresetCommand(bot, m)
		}
		return
	}

	if m.Document() != nil {
		startSessionFromFile(bot, m)
		return
	}

	if s := getActiveSessionByChat(m.ChatID()); s != nil && m.Text() != "" {
		switch s.State {
		case StateAwaitingPassword:
			handlePasswordReply(bot, m, s)
			return
		case StateAwaitingSearch:
			handleSearchReply(bot, m, s)
			return
		case StateAwaitingCustom:
			handleCustomReply(bot, m, s)
			return
		case StateAwaitingPresetSave:
			handlePresetSaveNameReply(bot, m, s)
			return
		case StateAwaitingPresetCreate:
			handlePresetCreateReply(bot, m, s)
			return
		}
	}

	if url := extractURL(m.Text()); url != "" {
		startSessionFromURL(bot, m, url)
		return
	}
}

var urlRe = regexp.MustCompile(`https?://[^\s)]+`)

func extractURL(text string) string {
	if text == "" {
		return ""
	}
	return urlRe.FindString(text)
}

func runCLIDownload(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: logs2cookies download <url> [parallel=20] [maxMB=0]")
		os.Exit(2)
	}
	url := args[0]
	parallel := DEFAULT_PARALLEL
	var maxBytes int64
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &parallel)
	}
	if len(args) > 2 {
		var mb int64
		fmt.Sscanf(args[2], "%d", &mb)
		maxBytes = mb * 1024 * 1024
	}
	dst := "download.bin"
	if u := strings.LastIndexAny(url, "/\\"); u != -1 {
		dst = url[u+1:]
		if i := strings.IndexByte(dst, '?'); i != -1 {
			dst = dst[:i]
		}
	}
	log.Printf("downloading %s -> %s (parallel=%d, maxMB=%d)", url, dst, parallel, maxBytes/(1024*1024))
	prog := func(d, total int64, bps float64) {
		pct := 0.0
		if total > 0 {
			pct = float64(d) / float64(total) * 100
		}
		log.Printf("  %s / %s  (%.1f%%)  @ %s/s", formatBytes(d), formatBytes(total), pct, formatBytes(int64(bps)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	res, err := parallelDownload(ctx, url, dst, parallel, maxBytes, prog)
	if err != nil {
		log.Fatalf("download failed: %v", err)
	}
	mbps := float64(res.Bytes) / 1024 / 1024 / res.Duration.Seconds()
	log.Printf("DONE: %s in %s — %.2f MB/s — parallel=%d range=%v ctype=%s",
		formatBytes(res.Bytes), res.Duration.Round(time.Millisecond), mbps, res.Parallel, res.RangeUsed, res.ContentType)
}

// parseCaptionMeta pulls intentional domain filter + archive password from a
// Telegram caption. Stealer packs often caption passwords like:
//
//	✅ Password @HUNTER_CLOUDS
//	pass: secret123
//
// Old behaviour treated the ENTIRE caption as a domain filter — so every cookie
// was dropped with "none matched ✅ Password @…". Only real filters stick now.
func parseCaptionMeta(caption string) (filter, password string) {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return "", ""
	}
	password = extractCaptionPassword(caption)
	filter = parseFilter(caption)
	return filter, password
}

func parseFilter(caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return ""
	}
	low := strings.ToLower(caption)

	// Explicit: filter:netflix  or  filter: steam epic
	if strings.HasPrefix(low, "filter:") {
		return cleanFilterToken(strings.TrimSpace(caption[len("filter:"):]))
	}

	// Whole caption is a password line → not a filter
	if looksLikePasswordCaption(caption) {
		return ""
	}

	// Only accept short domain-like tokens (no spaces/emoji/@/password noise).
	// Multi-word free text is almost never a filter; user can type domains later.
	if strings.ContainsAny(caption, " \t\n@#✅🔒🔑") {
		// allow "filter-like" multi domain only if every token is domain-ish
		parts := strings.FieldsFunc(caption, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
		})
		var good []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if isDomainFilterToken(p) {
				good = append(good, strings.ToLower(strings.TrimPrefix(p, ".")))
			}
		}
		if len(good) == 0 {
			return ""
		}
		// only if ALL tokens were domain-like (no junk mixed in)
		if len(good) == len(parts) {
			return strings.Join(good, " ")
		}
		return ""
	}

	if isDomainFilterToken(caption) {
		return cleanFilterToken(caption)
	}
	return ""
}

func cleanFilterToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, ".")
	return s
}

func isDomainFilterToken(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, ".")
	if s == "" || len(s) > 64 {
		return false
	}
	// password / UI junk
	for _, bad := range []string{"password", "pass", "passwd", "pwd", "pw", "hunter", "cloud"} {
		if s == bad {
			return false
		}
	}
	if strings.Contains(s, "password") || strings.Contains(s, "passwd") {
		return false
	}
	// must look like a hostname fragment: letters/digits/dots/hyphens only
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	// single pure number is not a domain
	if _, err := strconv.Atoi(s); err == nil {
		return false
	}
	return true
}

func looksLikePasswordCaption(caption string) bool {
	low := strings.ToLower(caption)
	// strip emoji / noise for matching
	compact := strings.Map(func(r rune) rune {
		if r < 128 {
			return r
		}
		return ' '
	}, low)
	compact = strings.Join(strings.Fields(compact), " ")
	if strings.Contains(compact, "password") || strings.Contains(compact, "passwd") {
		return true
	}
	if strings.HasPrefix(compact, "pass ") || strings.HasPrefix(compact, "pass:") ||
		strings.HasPrefix(compact, "pw:") || strings.HasPrefix(compact, "pwd:") ||
		strings.HasPrefix(compact, "pw ") || strings.HasPrefix(compact, "pwd ") {
		return true
	}
	return false
}

// extractCaptionPassword finds archive passwords in common stealer captions.
//
//	Password @HUNTER_CLOUDS
//	✅ Password: secret
//	pass: foo
//	pw=bar
func extractCaptionPassword(caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return ""
	}
	// Normalize fancy spaces / zero-width
	caption = strings.Map(func(r rune) rune {
		switch r {
		case '\u00a0', '\u200b', '\u200c', '\u200d', '\ufeff':
			return ' '
		default:
			return r
		}
	}, caption)

	patterns := []*regexp.Regexp{
		// Password @HUNTER_CLOUDS  /  Password: HUNTER  /  pw=xxx  /  pwd: xxx
		regexp.MustCompile(`(?i)(?:pass(?:word|wd)?|pwd|pw)\s*[@:=\-–—]\s*([^\s#|]+)`),
		// pass HUNTER_CLOUDS (space-separated)
		regexp.MustCompile(`(?i)(?:^|[\s|])(?:pass(?:word|wd)?|pwd|pw)\s+([A-Za-z0-9_@.\-]{3,})`),
		// 🔑 secret  /  🔒 secret
		regexp.MustCompile(`(?:🔑|🔒|🔐)\s*([A-Za-z0-9_@.\-]{3,})`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(caption); len(m) == 2 {
			pw := strings.TrimSpace(m[1])
			pw = strings.TrimPrefix(pw, "@")
			pw = strings.Trim(pw, "\"'`")
			if pw == "" || len(pw) < 3 {
				continue
			}
			if strings.EqualFold(pw, "password") || strings.EqualFold(pw, "pass") {
				continue
			}
			return pw
		}
	}
	return ""
}

func filterTag(f string) string {
	if f == "" {
		return ""
	}
	return " (filter=" + f + ")"
}

func looksLikeCookieFile(fullPath string) bool {
	p := strings.ToLower(fullPath)
	p = strings.ReplaceAll(p, "\\", "/")

	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}

	// Chrome/Edge Cookies DB (often extensionless or .db/.sqlite)
	if base == "cookies" || base == "cookies.db" || base == "cookies.sqlite" {
		return true
	}
	if strings.HasSuffix(base, ".sqlite") || strings.HasSuffix(base, ".db") {
		for _, h := range COOKIE_NAME_HINTS {
			if strings.Contains(base, h) {
				return true
			}
		}
		// profile path: .../Network/Cookies or .../Cookies
		if strings.Contains(p, "/network/cookies") || strings.HasSuffix(p, "/cookies") {
			return true
		}
	}

	// No extension: only match if the filename is exactly "cookies".
	if !strings.HasSuffix(p, ".txt") && !strings.HasSuffix(p, ".json") && !strings.HasSuffix(p, ".dat") &&
		!strings.HasSuffix(p, ".sqlite") && !strings.HasSuffix(p, ".db") {
		return base == "cookies"
	}

	// Path contains an explicit cookie-related keyword → always match.
	for _, h := range COOKIE_NAME_HINTS {
		if strings.Contains(p, h) {
			return true
		}
	}

	// .txt/.json/.dat file whose name (without extension) is a browser name
	// → likely a cookie dump named after the browser (chrome.txt, firefox.json…).
	stem := base
	for _, ext := range []string{".txt", ".json", ".dat", ".sqlite", ".db"} {
		stem = strings.TrimSuffix(stem, ext)
	}
	for _, b := range browserNames {
		if stem == b {
			return true
		}
	}

	return false
}

func detectBrowser(path string) string {
	lp := strings.ToLower(path)
	candidates := []string{"chrome", "edge", "firefox", "opera", "brave", "yandex", "vivaldi", "chromium", "gecko"}
	for _, c := range candidates {
		if strings.Contains(lp, c) {
			return c
		}
	}
	return "unknown"
}

func parseCookieFile(name string, data []byte) []CookieRow {
	if isSQLiteHeader(data) {
		return parseSQLiteCookies(name, data)
	}
	// extension hint for mis-detected empty sqlite
	ln := strings.ToLower(name)
	if strings.HasSuffix(ln, ".sqlite") || strings.HasSuffix(ln, ".db") {
		if rows := parseSQLiteCookies(name, data); len(rows) > 0 {
			return rows
		}
	}
	i := 0
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	if i < len(data) && (data[i] == '[' || data[i] == '{') {
		if rows := parseJSONCookies(name, data); len(rows) > 0 {
			return rows
		}
	}
	return parseNetscape(name, data)
}

func parseNetscape(source string, data []byte) []CookieRow {
	var out []CookieRow
	scn := bufio.NewScanner(bytes.NewReader(data))
	scn.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scn.Scan() {
		line := scn.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		raw := line
		if strings.HasPrefix(raw, "#HttpOnly_") {
			raw = strings.TrimPrefix(raw, "#HttpOnly_")
		}
		parts := strings.Split(raw, "\t")
		if len(parts) < 7 {
			continue
		}
		out = append(out, CookieRow{
			Domain:     parts[0],
			Flag:       parts[1],
			Path:       parts[2],
			Secure:     parts[3],
			Expiration: parts[4],
			Name:       parts[5],
			Value:      strings.Join(parts[6:], "\t"),
			Source:     source,
		})
	}
	return out
}

type jsonCookie struct {
	Domain         string      `json:"domain"`
	Path           string      `json:"path"`
	Name           string      `json:"name"`
	Value          string      `json:"value"`
	Secure         bool        `json:"secure"`
	HostOnly       bool        `json:"hostOnly"`
	HTTPOnly       bool        `json:"httpOnly"`
	ExpirationDate interface{} `json:"expirationDate"`
}

func parseJSONCookies(source string, data []byte) []CookieRow {
	var arr []jsonCookie
	if err := json.Unmarshal(data, &arr); err == nil {
		return jsonToRows(source, arr)
	}
	var obj struct {
		Cookies []jsonCookie `json:"cookies"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && len(obj.Cookies) > 0 {
		return jsonToRows(source, obj.Cookies)
	}
	return nil
}

func jsonToRows(source string, arr []jsonCookie) []CookieRow {
	rows := make([]CookieRow, 0, len(arr))
	for _, c := range arr {
		dom := c.Domain
		if dom == "" {
			continue
		}
		flag := "TRUE"
		if c.HostOnly {
			flag = "FALSE"
		}
		secure := "FALSE"
		if c.Secure {
			secure = "TRUE"
		}
		exp := "0"
		switch v := c.ExpirationDate.(type) {
		case float64:
			exp = fmt.Sprintf("%d", int64(v))
		case int64:
			exp = fmt.Sprintf("%d", v)
		case string:
			exp = v
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		rows = append(rows, CookieRow{
			Domain:     dom,
			Flag:       flag,
			Path:       path,
			Secure:     secure,
			Expiration: exp,
			Name:       c.Name,
			Value:      c.Value,
			Source:     source,
		})
	}
	return rows
}

func dedupe(rows []CookieRow) []CookieRow {
	seen := make(map[string]struct{}, len(rows))
	out := rows[:0]
	for _, r := range rows {
		k := r.Domain + "\x00" + r.Path + "\x00" + r.Name + "\x00" + r.Value
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// cleanSourceName turns a stealer-log path like
//
//	"5511_ON_CHANNEL/[USA][Win10]hash1/Chrome_Default/Network/Cookies.txt"
//
// into a tidy zip-entry stem like
//
//	"5511_ON_CHANNEL_USAWin10hash1_Chrome_Default".
//
// Strategy: keep ALL non-generic path segments joined by "_", so siblings
// that share a wrapper folder stay distinct via their unique sub-paths.
// (The previous "first non-generic segment" rule collapsed every victim
// under a shared wrapper into the same filename → all cookies mixed.)
func cleanSourceName(src string) string {
	src = strings.ReplaceAll(src, "\\", "/")
	parts := strings.Split(src, "/")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if hasCookieFileExt(last) {
			parts = parts[:len(parts)-1]
		}
	}

	generic := map[string]bool{
		"":     true,
		"logs": true, "log": true,
		"cookies": true, "cookie": true,
		"network": true, "default": true, "profile": true, "profiles": true,
		"data": true, "user data": true, "userdata": true,
		"browser": true, "browsers": true, "extension": true, "extensions": true,
		"local": true, "roaming": true, "appdata": true,
	}

	var keep []string
	for _, p := range parts {
		lp := strings.ToLower(strings.TrimSpace(p))
		if generic[lp] {
			continue
		}
		keep = append(keep, p)
	}

	var name string
	if len(keep) == 0 {
		name = "session"
	} else {
		name = strings.Join(keep, "_")
	}

	browser := detectBrowser(src)
	if browser != "unknown" && !strings.Contains(strings.ToLower(name), browser) {
		name = name + "_" + browser
	}

	out := safeFilename(name)
	if out == "" {
		out = "session"
	}
	return out
}

func hasCookieFileExt(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".txt") || strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".dat")
}

// safeFilename strips bad chars and caps length, preferring to keep the
// SUFFIX of long names — the rightmost segments are the most specific
// (browser/profile/victim hash) and most informative.
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if len(out) > 80 {
		out = out[len(out)-80:]
		out = strings.Trim(out, "._-")
	}
	return out
}

func writeNetscape(path string, rows []CookieRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "# Netscape HTTP Cookie File")
	fmt.Fprintln(w, "# Generated by logs2cookies-bot")
	fmt.Fprintln(w, "# https://curl.se/docs/http-cookies.html")
	fmt.Fprintln(w, "")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Domain, defaultStr(r.Flag, "TRUE"), defaultStr(r.Path, "/"),
			defaultStr(r.Secure, "FALSE"), defaultStr(r.Expiration, "0"),
			r.Name, r.Value,
		)
	}
	return w.Flush()
}

func writeJSON(path string, rows []CookieRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func normalDomain(d string) string {
	d = strings.TrimPrefix(d, ".")
	return strings.ToLower(d)
}

func summary(s Stats, filter, archive string) string {
	type kv struct {
		k string
		v int
	}
	doms := make([]kv, 0, len(s.DomainCounts))
	for k, v := range s.DomainCounts {
		doms = append(doms, kv{k, v})
	}
	sort.Slice(doms, func(i, j int) bool { return doms[i].v > doms[j].v })
	if len(doms) > 10 {
		doms = doms[:10]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "archive: %s\n", archive)
	fmt.Fprintf(&b, "files scanned: %d | cookie files: %d\n", s.ArchiveFiles, s.CookieFiles)
	fmt.Fprintf(&b, "cookies: %d total, %d unique%s\n", s.TotalCookies, s.UniqueCookies, filterTag(filter))
	if len(s.BrowserHints) > 0 {
		fmt.Fprintf(&b, "sources: ")
		first := true
		for k, v := range s.BrowserHints {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", k, v)
			first = false
		}
		b.WriteString("\n")
	}
	if len(doms) > 0 {
		b.WriteString("top domains:\n")
		for _, d := range doms {
			fmt.Fprintf(&b, "  %s — %d\n", d.k, d.v)
		}
	}
	return b.String()
}

func helpText() string {
	builtins := strings.Join(listBuiltinPresetNames(), " · ")
	return strings.Join([]string{
		"🍪 *logs2cookies*",
		"extract cookies from stealer-log archives.",
		"",
		"*send me*",
		"  📎 a `.zip` / `.rar` file (up to `2 GB`)",
		"  📎 split archive? send parts then `/done` — *incomplete sets still extract*",
		"  🔗 or paste a direct URL to a `.zip`/`.rar` (up to `5 GB`)",
		"",
		"*then pick what you want*",
		"  • tap *extract all*, *top 50*, or *browse domains*",
		"  • tap a *builtin pack* (cursor · github · steam…)",
		"  • tap *⭐ presets* to save/reuse domain sets",
		"  • or just type domains: `netflix paypal steam`",
		"",
		"*builtin packs*",
		"  " + builtins,
		"",
		"*tips*",
		"  🔒 encrypted? i'll ask for the password",
		"  🪆 nested archives unpacked automatically",
		"  🏷 caption `filter:netflix` to pre-filter during extraction",
		"  🧹 expired + empty values dropped automatically",
		"  💾 Chrome/Firefox SQLite cookie DBs parsed too",
		"  ⭐ `/preset save gaming steam epicgames` — reuse across archives",
		"",
		"`/cancel` — abort  ·  `/done` — finish multi-part rar  ·  `/preset` — manage presets",
	}, "\n")
}

func reply(bot *Bot, m *telegram.NewMessage, text string) {
	if _, err := m.Reply(text, sendOpts()); err != nil {
		log.Printf("reply chat=%d failed: %v", m.ChatID(), err)
	}
}

func editStatus(bot *Bot, s SentMsg, text string) {
	bot.EditStatus(s, text)
}

// editStatusCard uses HTML progress cards (safe for _ ! @ in filenames).
func editStatusCard(bot *Bot, s SentMsg, text string) {
	bot.EditStatusHTML(s, text)
}

type progressFn func(done, total int64, bps float64)

type progressReader struct {
	r        io.Reader
	total    int64
	done     int64
	cb       progressFn
	start    time.Time
	lastEdit time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		if p.cb != nil && time.Since(p.lastEdit) >= 2*time.Second {
			elapsed := time.Since(p.start).Seconds()
			bps := 0.0
			if elapsed > 0 {
				bps = float64(p.done) / elapsed
			}
			p.cb(p.done, p.total, bps)
			p.lastEdit = time.Now()
		}
	}
	return n, err
}

func (p *progressReader) flush() {
	if p.cb != nil {
		elapsed := time.Since(p.start).Seconds()
		bps := 0.0
		if elapsed > 0 {
			bps = float64(p.done) / elapsed
		}
		p.cb(p.done, p.total, bps)
	}
}

var janMu sync.Mutex

func janitor(root string) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		janMu.Lock()
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > WORK_TTL {
				os.RemoveAll(filepath.Join(root, e.Name()))
			}
		}
		janMu.Unlock()
	}
}
