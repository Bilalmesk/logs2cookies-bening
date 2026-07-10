package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Status cards for Telegram — HTML + <pre> so long filenames with _ ! @
// never break markdown, and the progress bar stays monospace-aligned on mobile.

func htmlEsc(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
	).Replace(s)
}

// shortName keeps the bar readable on phone screens. Prefers the unique
// suffix of stealer pack names (…ON_CHANNEL.rar) over the long prefix.
func shortName(name string, max int) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" {
		return "archive"
	}
	if utf8.RuneCountInString(name) <= max {
		return name
	}
	runes := []rune(name)
	ext := filepath.Ext(name)
	extRunes := []rune(ext)
	// "head…tail.ext"
	budget := max - len(extRunes) - 1 // for …
	if budget < 8 {
		return string(runes[:max-1]) + "…"
	}
	head := budget / 3
	if head < 4 {
		head = 4
	}
	tail := budget - head
	stem := runes
	if len(extRunes) > 0 && len(runes) > len(extRunes) {
		stem = runes[:len(runes)-len(extRunes)]
	}
	if len(stem) <= budget {
		return string(stem) + ext
	}
	return string(stem[:head]) + "…" + string(stem[len(stem)-tail:]) + ext
}

func progressBarPlain(pct float64, width int) string {
	if width < 4 {
		width = 4
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(float64(width)*pct/100 + 0.5)
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func statusStep(step, total int, icon, title string) string {
	return fmt.Sprintf("<b>[%d/%d]</b> %s <b>%s</b>", step, total, icon, htmlEsc(title))
}

// statusDownload — aligned progress card for TG / URL downloads.
func statusDownload(name string, done, total int64, bps float64) string {
	pct := 0.0
	if total > 0 {
		pct = float64(done) / float64(total) * 100
	}
	eta := "—"
	if bps > 1 && total > done {
		eta = fmtDuration(float64(total-done) / bps)
	}
	var b strings.Builder
	b.WriteString(statusStep(1, 3, "⬇️", "downloading"))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "📦 <code>%s</code>\n", htmlEsc(shortName(name, 40)))
	b.WriteString("<pre>")
	fmt.Fprintf(&b, "%s %5.1f%%\n", progressBarPlain(pct, 14), pct)
	fmt.Fprintf(&b, "%-10s / %s\n", formatBytes(done), formatBytes(total))
	fmt.Fprintf(&b, "%-10s · ETA %s", formatBytes(int64(bps))+"/s", eta)
	b.WriteString("</pre>")
	return b.String()
}

func statusDownloadStart(name string) string {
	var b strings.Builder
	b.WriteString(statusStep(1, 3, "⬇️", "downloading"))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "📦 <code>%s</code>\n", htmlEsc(shortName(name, 40)))
	b.WriteString("<pre>")
	fmt.Fprintf(&b, "%s %5.1f%%\n", progressBarPlain(0, 14), 0.0)
	b.WriteString("starting…")
	b.WriteString("</pre>")
	return b.String()
}

func statusDownloadDone(size int64, elapsed string, parallel int) string {
	var b strings.Builder
	b.WriteString(statusStep(1, 3, "✅", "downloaded"))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "<pre>%-10s in %s", formatBytes(size), elapsed)
	if parallel > 1 {
		fmt.Fprintf(&b, " · %dx", parallel)
	}
	b.WriteString("</pre>\n")
	b.WriteString(statusStep(2, 3, "⚙️", "extracting…"))
	return b.String()
}

func statusQueued(ahead int, elapsed string) string {
	var b strings.Builder
	b.WriteString(statusStep(2, 3, "⏳", "queued"))
	b.WriteByte('\n')
	b.WriteString("<pre>")
	if ahead > 0 {
		fmt.Fprintf(&b, "%d job(s) ahead\n", ahead)
	}
	fmt.Fprintf(&b, "waited %s", elapsed)
	b.WriteString("</pre>")
	return b.String()
}

func statusExtractStarting(elapsed string) string {
	var b strings.Builder
	b.WriteString(statusStep(2, 3, "⚙️", "starting extract"))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "<pre>elapsed %s</pre>", elapsed)
	return b.String()
}

func statusExtracting(spin string, entries, cookies int, eps, cps float64, elapsed string) string {
	var b strings.Builder
	b.WriteString(statusStep(2, 3, spin, "extracting"))
	b.WriteByte('\n')
	b.WriteString("<pre>")
	fmt.Fprintf(&b, "files   %8s  (%s/s)\n", commafy(entries), commafy(int(eps)))
	fmt.Fprintf(&b, "cookies %8s  (%s/s)\n", commafy(cookies), commafy(int(cps)))
	fmt.Fprintf(&b, "time    %8s", elapsed)
	b.WriteString("</pre>")
	return b.String()
}

func statusPacking(label string, i, n int) string {
	var b strings.Builder
	b.WriteString(statusStep(3, 3, "📦", "packing"))
	b.WriteByte('\n')
	if n > 1 {
		fmt.Fprintf(&b, "<code>%s</code>\n", htmlEsc(shortName(label, 40)))
		fmt.Fprintf(&b, "<pre>%s  %d / %d\n%s</pre>",
			progressBarPlain(float64(i)/float64(n)*100, 14), i, n, progressDots(i, n))
	} else {
		fmt.Fprintf(&b, "<code>%s</code>", htmlEsc(shortName(label, 40)))
	}
	return b.String()
}

func statusPackingCombined() string {
	return statusStep(3, 3, "📦", "packing") + "\n<pre>building cookies.zip…</pre>"
}

func statusMultipart(kind string, names []string, partial string) string {
	var b strings.Builder
	b.WriteString(statusStep(2, 3, "⚙️", "extracting multi-part "+kind))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "<b>%d</b> part(s)\n", len(names))
	b.WriteString("<pre>")
	for i, n := range names {
		if i >= 8 {
			fmt.Fprintf(&b, "… +%d more\n", len(names)-8)
			break
		}
		fmt.Fprintf(&b, "• %s\n", shortName(n, 36))
	}
	b.WriteString("</pre>")
	if partial != "" {
		// partial is already markdown-ish; strip to plain for HTML card
		b.WriteString("\n")
		b.WriteString(htmlEsc(partial))
	} else {
		b.WriteString("\n<i>any subset of parts is OK — best-effort</i>")
	}
	return b.String()
}
