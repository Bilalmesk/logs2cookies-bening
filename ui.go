package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/amarnathcjd/gogram/telegram"
)

// friendlyError maps an internal error to a one-line, action-oriented user
// message. Stealer logs are messy — encrypted inner files, truncated downloads,
// RAR volume gaps — so every message tells the user the next move, not just the
// problem. Falls back to the raw error string when no pattern matches.
func friendlyError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	switch {
	case errors.Is(err, ErrPasswordRequired):
		return "🔒 *password required* — reply with the archive password"
	case errors.Is(err, ErrBadPassword):
		return "❌ *wrong password* — reply with the correct one"
	case errors.Is(err, ErrRarPartsMissing):
		return "📎 " + msg + "\n\n" +
			"send more parts if you have them, then `/done`.\n" +
			"incomplete sets still try best-effort (install `7zz` on the host for mid-part recovery)"
	case strings.Contains(low, "cloudflare"):
		return "🛡 download blocked by Cloudflare\npaste the *direct* final `.zip`/`.rar` URL"
	case strings.Contains(low, "not a zip/rar") || strings.Contains(low, "not a zip") || strings.Contains(low, "not a rar"):
		return "❌ URL didn't return a `.zip`/`.rar`\nthat page is probably HTML (login/redirect) — paste the direct file URL"
	case strings.Contains(low, "file too large"):
		return "📦 " + msg
	case strings.Contains(low, "context deadline exceeded"), strings.Contains(low, "timeout"):
		return "⌛ download timed out — server too slow or link dead\ntry again or use a smaller archive"
	case strings.Contains(low, "no such host"), strings.Contains(low, "connection refused"), strings.Contains(low, "no connection"):
		return "🌐 can't reach that host — check the URL or try again later"
	case strings.Contains(low, "status 4"):
		return fmt.Sprintf("🚫 server rejected the download (`%s`)\nlink may be expired or private", msg)
	case strings.Contains(low, "status 5"):
		return "🔧 server error on their end — try again in a minute"
	}
	return "❌ " + msg
}

// noCookiesMessage explains WHY extraction found nothing — way more useful
// than a bare "no cookies found" which leaves the user guessing. It inspects
// the stats gathered during extraction to point at the likely cause.
func noCookiesMessage(stats Stats, filter string) string {
	var b strings.Builder
	b.WriteString("🤷 *no cookies extracted*")
	b.WriteString(filterTag(filter))

	switch {
	case stats.ArchiveFiles == 0:
		b.WriteString("\n\n📂 the archive was *empty or corrupt* — nothing inside to scan")
	case stats.CookieFiles == 0:
		b.WriteString("\n\n🔍 scanned ")
		b.WriteString(commafy(stats.ArchiveFiles))
		b.WriteString(" file(s) — *none looked like cookie files*")
		b.WriteString("\nthis archive may be: passwords only · autostart info · not a stealer log")
	case filter != "":
		b.WriteString("\n\n🔎 found ")
		b.WriteString(commafy(stats.CookieFiles))
		b.WriteString(" cookie file(s) with ")
		b.WriteString(commafy(stats.TotalCookies))
		b.WriteString(" cookies, but *none matched* `")
		b.WriteString(escapeMd(filter))
		b.WriteString("`")
		b.WriteString("\ntry `filter:` off, or a broader substring")
	default:
		b.WriteString("\n\ncookie files were present but parsed empty")
	}
	return b.String()
}

// progressDots renders a compact `●●●○○` progress indicator for short flows
// where a full bar is overkill. Used by the packing step.
func progressDots(done, total int) string {
	if total <= 0 {
		return ""
	}
	if done > total {
		done = total
	}
	if done < 0 {
		done = 0
	}
	var b strings.Builder
	for i := 0; i < done; i++ {
		b.WriteString("●")
	}
	for i := done; i < total; i++ {
		b.WriteString("○")
	}
	return b.String()
}

// retryKeyboard returns an inline keyboard with a single "↻ retry" button
// wired to the session. Used when a URL download fails so the user can retry
// without re-pasting the URL.
func retryKeyboard(sessionID string) telegram.ReplyMarkup {
	return inlineKeyboard([]telegram.KeyboardButton{
		cbBtn("↻ retry", "retry:"+sessionID),
		cbBtn("❌ dismiss", "c:"+sessionID),
	})
}
