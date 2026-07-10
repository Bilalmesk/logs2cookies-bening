package main

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestDomainMatchesTarget(t *testing.T) {
	cases := []struct {
		cookie, target string
		want           bool
	}{
		{".cursor.com", "cursor.com", true},
		{"cursor.com", "cursor.com", true},
		{"api.cursor.com", "cursor.com", true},
		{"notcursor.com", "cursor.com", false},
		{"cursor.com.evil.tld", "cursor.com", false},
		{"accounts.github.com", "github.com", true},
		{"githubusercontent.com", "github.com", false},
	}
	for _, c := range cases {
		got := domainMatchesTarget(c.cookie, c.target)
		if got != c.want {
			t.Errorf("domainMatchesTarget(%q,%q)=%v want %v", c.cookie, c.target, got, c.want)
		}
	}
}

func TestQualityDropsEmptyAndExpired(t *testing.T) {
	// reset CLI flags
	cliKeepEmpty, cliKeepExpired = false, false
	cliNameFilter, cliDomainFilter = nil, nil
	cliStrictDomains = false

	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "s")
	sp, err := NewSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	future := fmt.Sprintf("%d", time.Now().Unix()+86400)
	past := fmt.Sprintf("%d", time.Now().Unix()-86400)

	sp.Add(CookieRow{Domain: "a.com", Path: "/", Name: "ok", Value: "v", Expiration: future, Source: "s1"})
	sp.Add(CookieRow{Domain: "a.com", Path: "/", Name: "empty", Value: "", Expiration: future, Source: "s1"})
	sp.Add(CookieRow{Domain: "a.com", Path: "/", Name: "deleted", Value: "deleted", Expiration: future, Source: "s1"})
	sp.Add(CookieRow{Domain: "a.com", Path: "/", Name: "old", Value: "v", Expiration: past, Source: "s1"})
	sp.Close()

	rows, err := readSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 good cookie, got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "ok" {
		t.Errorf("got name %q", rows[0].Name)
	}
}

func TestBuiltinPresetFilter(t *testing.T) {
	p := getBuiltinPreset("cursor")
	if p == nil {
		t.Fatal("missing cursor preset")
	}
	rows := []CookieRow{
		{Domain: ".cursor.com", Name: "WorkosCursorSessionToken", Value: "tok"},
		{Domain: ".cursor.com", Name: "NID", Value: "nope"},
		{Domain: "evil.com", Name: "WorkosCursorSessionToken", Value: "nope"},
	}
	out := filterRowsByPreset(rows, p)
	if len(out) != 1 {
		t.Fatalf("want 1, got %d", len(out))
	}
	if out[0].Value != "tok" {
		t.Errorf("wrong row: %+v", out[0])
	}
}

func TestSQLiteChromeCookies(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "Cookies")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE cookies (
  host_key TEXT, path TEXT, is_secure INTEGER, expires_utc INTEGER,
  name TEXT, value TEXT, is_httponly INTEGER
);
INSERT INTO cookies VALUES ('.cursor.com', '/', 1, 13300000000000000, 'WorkosCursorSessionToken', 'abc', 1);
INSERT INTO cookies VALUES ('github.com', '/', 1, 13300000000000000, 'user_session', 'gh', 0);
`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rows := parseSQLiteCookies("Chrome/Cookies", data)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// chrome time should normalize to reasonable unix
	if cookieExpirationUnix(rows[0].Expiration) < 1_000_000_000 {
		t.Errorf("expiration not normalized: %s", rows[0].Expiration)
	}
}

func TestProcessZip_WithQualityAndSQLite(t *testing.T) {
	cliKeepEmpty, cliKeepExpired = false, false
	cliNameFilter, cliDomainFilter = nil, nil
	cliStrictDomains = false

	dir := t.TempDir()
	// build sqlite blob
	dbPath := filepath.Join(dir, "Cookies")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	futureChrome := (time.Now().Unix()+chromeEpochOffset)*1_000_000 + 1
	_, err = db.Exec(`
CREATE TABLE cookies (
  host_key TEXT, path TEXT, is_secure INTEGER, expires_utc INTEGER,
  name TEXT, value TEXT, is_httponly INTEGER
);
INSERT INTO cookies VALUES ('.steamcommunity.com', '/', 1, ?, 'steamLoginSecure', 'steamval', 1);
`, futureChrome)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	sqliteData, _ := os.ReadFile(dbPath)

	zipPath := filepath.Join(dir, "sample.zip")
	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	future := fmt.Sprintf("%d", time.Now().Unix()+99999)
	past := fmt.Sprintf("%d", time.Now().Unix()-99999)
	netscape := "# Netscape HTTP Cookie File\n" +
		".netflix.com\tTRUE\t/\tTRUE\t" + future + "\tNetflixId\tgood\n" +
		".netflix.com\tTRUE\t/\tTRUE\t" + past + "\tNetflixId\texpired\n" +
		".netflix.com\tTRUE\t/\tTRUE\t" + future + "\tjunk\tdeleted\n"
	w, _ := zw.Create("victim1/Cookies.txt")
	w.Write([]byte(netscape))
	w2, _ := zw.Create("victim2/Chrome/Network/Cookies")
	w2.Write(sqliteData)
	zw.Close()
	zf.Close()

	rows, stats, err := processArchive(zipPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// good netflix + steam from sqlite; expired+deleted dropped
	if len(rows) < 2 {
		t.Fatalf("want >=2 rows, got %d (%+v)", len(rows), rows)
	}
	if stats.CookieFiles < 2 {
		t.Errorf("cookie files: %d", stats.CookieFiles)
	}
	// filter steam
	rows2, _, _ := processArchive(zipPath, "steamcommunity.com", "")
	if len(rows2) != 1 {
		t.Errorf("steam filter: want 1 got %d", len(rows2))
	}
}
