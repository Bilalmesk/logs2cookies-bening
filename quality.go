package main

import (
	"strconv"
	"strings"
	"time"
)

// Quality filters applied at spool-write time so junk never hits disk.

func isEmptyCookieValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "deleted", "null", "undefined", "none", "-":
		return true
	}
	return false
}

// cookieExpirationUnix parses Netscape/JSON expiration fields into unix seconds.
// Returns 0 for session cookies / unparseable.
func cookieExpirationUnix(exp string) int64 {
	exp = strings.TrimSpace(exp)
	if exp == "" || exp == "0" {
		return 0
	}
	// float from JSON expirationDate
	if f, err := strconv.ParseFloat(exp, 64); err == nil {
		if f > 1e15 { // chrome µs
			return chromeTimeToUnix(int64(f))
		}
		if f > 1e12 { // ms
			return int64(f / 1000)
		}
		return int64(f)
	}
	return 0
}

const chromeEpochOffset = 11644473600 // seconds 1601 → 1970

func chromeTimeToUnix(expiresUTC int64) int64 {
	if expiresUTC <= 0 {
		return 0
	}
	if expiresUTC > 10_000_000_000_000 { // µs since 1601
		u := expiresUTC/1_000_000 - chromeEpochOffset
		if u < 0 {
			return 0
		}
		return u
	}
	if expiresUTC > 10_000_000_000 { // ms
		return expiresUTC / 1000
	}
	return expiresUTC
}

// isExpiredCookie returns true when the cookie has a positive expiry in the past.
// Session cookies (exp == 0) are kept.
func isExpiredCookie(exp string, now int64) bool {
	u := cookieExpirationUnix(exp)
	if u <= 0 {
		return false
	}
	return u < now
}

// domainMatchesTarget is subdomain-aware:
//
//	target "cursor.com" matches ".cursor.com", "cursor.com", "api.cursor.com"
//	but NOT "notcursor.com" or "cursor.com.evil.tld"
func domainMatchesTarget(cookieDomain, target string) bool {
	cd := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(cookieDomain), "."))
	td := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(target), "."))
	if cd == "" || td == "" {
		return false
	}
	if cd == td {
		return true
	}
	return strings.HasSuffix(cd, "."+td)
}

// domainContainsLoose keeps the old substring behaviour for free-typed search
// ("steam" → steamcommunity.com) while still being used only for custom filters.
func domainContainsLoose(cookieDomain, needle string) bool {
	cd := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(cookieDomain), "."))
	n := strings.ToLower(strings.TrimSpace(needle))
	if n == "" {
		return false
	}
	return strings.Contains(cd, n)
}

// cookieNameMatches is case-insensitive exact name match against a set.
func cookieNameMatches(name string, names []string) bool {
	if len(names) == 0 {
		return true
	}
	ln := strings.ToLower(strings.TrimSpace(name))
	for _, n := range names {
		if ln == strings.ToLower(n) {
			return true
		}
	}
	return false
}

// passesQuality is the default write-time gate (empty + expired).
func passesQuality(r CookieRow, now int64) bool {
	if isEmptyCookieValue(r.Value) {
		return false
	}
	if isExpiredCookie(r.Expiration, now) {
		return false
	}
	if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Domain) == "" {
		return false
	}
	return true
}

// normalizeExpirationField rewrites Chrome-style huge timestamps to unix seconds
// so Netscape output is importable by browsers/tools.
func normalizeExpirationField(exp string) string {
	u := cookieExpirationUnix(exp)
	if u <= 0 {
		return "0"
	}
	return strconv.FormatInt(u, 10)
}

func nowUnix() int64 { return time.Now().Unix() }
