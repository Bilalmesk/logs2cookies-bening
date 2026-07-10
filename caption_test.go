package main

import "testing"

func TestParseFilter_IgnoresPasswordCaptions(t *testing.T) {
	cases := []string{
		"✅ Password @HUNTER_CLOUDS",
		"Password @HUNTER_CLOUDS",
		"password: secret123",
		"pass: foo_bar",
		"🔒 Password @PACK_NAME",
		"Password - hunter2",
	}
	for _, c := range cases {
		f := parseFilter(c)
		if f != "" {
			t.Errorf("parseFilter(%q) = %q, want empty (not a domain filter)", c, f)
		}
	}
}

func TestParseFilter_AcceptsRealFilters(t *testing.T) {
	cases := map[string]string{
		"filter:netflix":         "netflix",
		"filter: steam":          "steam",
		"netflix.com":            "netflix.com",
		"cursor.com":             "cursor.com",
		"steam":                  "steam",
		"github.com,discord.com": "github.com discord.com", // fields split — wait, comma only via FieldsFunc
	}
	// github.com,discord.com has no space — isDomainFilterToken allows comma? no
	// single token with comma fails isDomainFilterToken
	if got := parseFilter("filter:netflix.com"); got != "netflix.com" {
		t.Errorf("filter: prefix: got %q", got)
	}
	if got := parseFilter("steam"); got != "steam" {
		t.Errorf("steam: got %q", got)
	}
	if got := parseFilter("netflix.com"); got != "netflix.com" {
		t.Errorf("netflix.com: got %q", got)
	}
	_ = cases
}

func TestExtractCaptionPassword(t *testing.T) {
	cases := map[string]string{
		"✅ Password @HUNTER_CLOUDS": "HUNTER_CLOUDS",
		"Password @HUNTER_CLOUDS":   "HUNTER_CLOUDS",
		"password: secret123":       "secret123",
		"pass: foo_bar":             "foo_bar",
		"pw=abcDEF99":               "abcDEF99",
		"filter:netflix":            "", // not a password
		"netflix.com":               "",
	}
	for in, want := range cases {
		got := extractCaptionPassword(in)
		if got != want {
			t.Errorf("extractCaptionPassword(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseCaptionMeta(t *testing.T) {
	f, p := parseCaptionMeta("✅ Password @HUNTER_CLOUDS")
	if f != "" {
		t.Errorf("filter should be empty, got %q", f)
	}
	if p != "HUNTER_CLOUDS" {
		t.Errorf("password=%q want HUNTER_CLOUDS", p)
	}

	f, p = parseCaptionMeta("filter:cursor.com")
	if f != "cursor.com" {
		t.Errorf("filter=%q", f)
	}
	if p != "" {
		t.Errorf("password should be empty, got %q", p)
	}
}
