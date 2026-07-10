package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// parseSQLiteCookies reads a Chrome/Edge/Chromium Cookies DB (or similar
// host_key/name/value schema). Values may be empty when encrypted — those are
// dropped by quality filters later.
func parseSQLiteCookies(source string, data []byte) []CookieRow {
	if len(data) < 16 || !bytes.HasPrefix(data, []byte("SQLite format 3")) {
		return nil
	}

	tmp, err := os.CreateTemp("", "cookies-*.db")
	if err != nil {
		return nil
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil
	}
	tmp.Close()

	// mode=ro + immutable reduces lock issues on some dumps
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)", tmpPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil
	}
	defer db.Close()

	// Prefer Chromium schema; fall back to Mozilla if present.
	if rows := queryChromiumCookies(source, db); len(rows) > 0 {
		return rows
	}
	return queryMozillaCookies(source, db)
}

func queryChromiumCookies(source string, db *sql.DB) []CookieRow {
	q := `
SELECT host_key, path, is_secure, expires_utc, name,
       COALESCE(value, ''), COALESCE(is_httponly, 0)
FROM cookies
WHERE name IS NOT NULL AND name != ''
`
	rs, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer rs.Close()

	var out []CookieRow
	for rs.Next() {
		var host, path, name, value string
		var isSecure, isHTTPOnly int
		var expiresUTC int64
		if err := rs.Scan(&host, &path, &isSecure, &expiresUTC, &name, &value, &isHTTPOnly); err != nil {
			continue
		}
		if host == "" || name == "" {
			continue
		}
		if path == "" {
			path = "/"
		}
		secure := "FALSE"
		if isSecure != 0 {
			secure = "TRUE"
		}
		flag := "TRUE" // include subdomains if host starts with .
		if !strings.HasPrefix(host, ".") {
			flag = "FALSE"
		}
		out = append(out, CookieRow{
			Domain:     host,
			Flag:       flag,
			Path:       path,
			Secure:     secure,
			Expiration: fmt.Sprintf("%d", chromeTimeToUnix(expiresUTC)),
			Name:       name,
			Value:      value,
			Source:     source,
		})
	}
	return out
}

func queryMozillaCookies(source string, db *sql.DB) []CookieRow {
	// Firefox cookies.sqlite
	q := `
SELECT host, path, isSecure, expiry, name, value, isHttpOnly
FROM moz_cookies
WHERE name IS NOT NULL AND name != ''
`
	rs, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer rs.Close()

	var out []CookieRow
	for rs.Next() {
		var host, path, name, value string
		var isSecure, isHTTPOnly int
		var expiry int64
		if err := rs.Scan(&host, &path, &isSecure, &expiry, &name, &value, &isHTTPOnly); err != nil {
			continue
		}
		if host == "" || name == "" {
			continue
		}
		if path == "" {
			path = "/"
		}
		secure := "FALSE"
		if isSecure != 0 {
			secure = "TRUE"
		}
		flag := "TRUE"
		if !strings.HasPrefix(host, ".") {
			flag = "FALSE"
		}
		out = append(out, CookieRow{
			Domain:     host,
			Flag:       flag,
			Path:       path,
			Secure:     secure,
			Expiration: fmt.Sprintf("%d", expiry),
			Name:       name,
			Value:      value,
			Source:     source,
		})
	}
	return out
}

func isSQLiteHeader(data []byte) bool {
	return len(data) >= 16 && bytes.HasPrefix(data, []byte("SQLite format 3"))
}
