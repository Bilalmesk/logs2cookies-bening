package main

import (
	"errors"
	"strings"
	"testing"
)

func TestFriendlyError_KnownPatterns(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"password required", ErrPasswordRequired, "password required"},
		{"bad password", ErrBadPassword, "wrong password"},
		{"cloudflare", errors.New("cloudflare challenge blocked this download"), "Cloudflare"},
		{"not zip", errors.New("downloaded response is not a zip/rar archive"), "didn't return a"},
		{"too large", errors.New("file too large (6GB, cap 5GB)"), "6GB"},
		{"timeout", errors.New("context deadline exceeded"), "timed out"},
		{"4xx", errors.New("status 404"), "rejected"},
		{"5xx", errors.New("status 503"), "server error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := friendlyError(c.err)
			if !strings.Contains(got, c.want) {
				t.Errorf("friendlyError(%v):\n  got:  %q\n  want substring: %q", c.err, got, c.want)
			}
		})
	}
}

func TestFriendlyError_Fallback(t *testing.T) {
	err := errors.New("some weird unmapped error")
	got := friendlyError(err)
	if !strings.HasPrefix(got, "❌ ") {
		t.Errorf("fallback should be prefixed with ❌, got: %q", got)
	}
	if !strings.Contains(got, "some weird unmapped error") {
		t.Errorf("fallback should preserve raw message, got: %q", got)
	}
}

func TestFriendlyError_Nil(t *testing.T) {
	if got := friendlyError(nil); got != "" {
		t.Errorf("friendlyError(nil) should be empty, got: %q", got)
	}
}

func TestNoCookiesMessage_Diagnostics(t *testing.T) {
	// Empty/corrupt archive.
	got := noCookiesMessage(Stats{}, "")
	if !strings.Contains(got, "empty or corrupt") {
		t.Errorf("empty archive: want 'empty or corrupt' hint, got: %s", got)
	}

	// Files scanned but none look like cookies.
	got = noCookiesMessage(Stats{ArchiveFiles: 42, CookieFiles: 0}, "")
	if !strings.Contains(got, "42") || !strings.Contains(got, "none looked like cookie") {
		t.Errorf("no cookie files: want count + hint, got: %s", got)
	}

	// Cookies found but none matched filter.
	got = noCookiesMessage(Stats{ArchiveFiles: 100, CookieFiles: 5, TotalCookies: 200}, "netflix")
	if !strings.Contains(got, "none matched") || !strings.Contains(got, "netflix") {
		t.Errorf("filter mismatch: want 'none matched' + filter name, got: %s", got)
	}
}

func TestProgressDots(t *testing.T) {
	if got := progressDots(0, 0); got != "" {
		t.Errorf("zero total should be empty, got %q", got)
	}
	if got := progressDots(2, 5); got != "●●○○○" {
		t.Errorf("2/5 want '●●○○○', got %q", got)
	}
	if got := progressDots(5, 5); got != "●●●●●" {
		t.Errorf("5/5 want '●●●●●', got %q", got)
	}
	// Clamps overflow.
	if got := progressDots(7, 5); got != "●●●●●" {
		t.Errorf("overflow should clamp, got %q", got)
	}
}
