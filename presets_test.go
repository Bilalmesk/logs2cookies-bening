package main

import (
	"testing"
)

func TestPresetStore_SaveAndGet(t *testing.T) {
	p := &PresetStore{data: map[int64]map[string][]string{}}

	got, err := p.Save(1, "gaming", []string{"steam", "epicgames", "riot"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("Save returned %d domains, want 3", len(got))
	}

	doms := p.Get(1, "gaming")
	if len(doms) != 3 {
		t.Fatalf("Get: %d domains, want 3", len(doms))
	}
	// Lookup is case-insensitive on name.
	if d := p.Get(1, "GAMING"); d == nil {
		t.Error("case-insensitive name lookup failed")
	}
	// Lookup by name is lowercased internally.
	if d := p.Get(1, "Gaming"); len(d) != 3 {
		t.Error("mixed-case name lookup failed")
	}
}

func TestPresetStore_SaveValidation(t *testing.T) {
	p := &PresetStore{data: map[int64]map[string][]string{}}

	if _, err := p.Save(1, "", []string{"x.com"}); err == nil {
		t.Error("empty name should error")
	}
	if _, err := p.Save(1, "x", nil); err == nil {
		t.Error("empty domains should error")
	}
	if _, err := p.Save(1, "x", []string{"  "}); err == nil {
		t.Error("whitespace-only domains should error")
	}
}

func TestPresetStore_List(t *testing.T) {
	p := &PresetStore{data: map[int64]map[string][]string{}}
	p.Save(1, "zebra", []string{"a.com"})
	p.Save(1, "alpha", []string{"b.com"})
	p.Save(1, "mango", []string{"c.com"})

	got := p.List(1)
	if len(got) != 3 {
		t.Fatalf("List: %d, want 3", len(got))
	}
	// Must be sorted.
	want := []string{"alpha", "mango", "zebra"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("List[%d] = %q, want %q (not sorted?)", i, got[i], w)
		}
	}

	// Empty for unknown user.
	if l := p.List(999); l != nil && len(l) != 0 {
		t.Errorf("unknown user List should be empty, got %v", l)
	}
}

func TestPresetStore_Delete(t *testing.T) {
	p := &PresetStore{data: map[int64]map[string][]string{}}
	p.Save(1, "x", []string{"a.com"})

	if !p.Delete(1, "x") {
		t.Error("Delete existing should return true")
	}
	if p.Get(1, "x") != nil {
		t.Error("preset still present after delete")
	}
	if p.Delete(1, "x") {
		t.Error("Delete twice should return false")
	}
	// Case-insensitive.
	p.Save(1, "y", []string{"b.com"})
	if !p.Delete(1, "Y") {
		t.Error("Delete should be case-insensitive")
	}
}

func TestPresetStore_PerUserIsolation(t *testing.T) {
	p := &PresetStore{data: map[int64]map[string][]string{}}
	p.Save(1, "shared-name", []string{"user1.com"})
	p.Save(2, "shared-name", []string{"user2.com"})

	if d := p.Get(1, "shared-name"); len(d) != 1 || d[0] != "user1.com" {
		t.Errorf("user 1 saw wrong data: %v", d)
	}
	if d := p.Get(2, "shared-name"); len(d) != 1 || d[0] != "user2.com" {
		t.Errorf("user 2 saw wrong data: %v", d)
	}
}

func TestNormalizeDomains(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"simple", []string{"Steam.com"}, []string{"steam.com"}},
		{"split on spaces", []string{"steam epicgames"}, []string{"steam", "epicgames"}},
		{"split on commas", []string{"steam,epicgames"}, []string{"steam", "epicgames"}},
		{"mixed delimiters", []string{"steam, epicgames; riot\nvalve"}, []string{"steam", "epicgames", "riot", "valve"}},
		{"dedupe", []string{"steam steam STEAM"}, []string{"steam"}},
		{"trim whitespace", []string{"  steam  "}, []string{"steam"}},
		{"drop empties", []string{"steam", "", "  ", "epic"}, []string{"steam", "epic"}},
		{"preserve first-seen order", []string{"zebra alpha mango"}, []string{"zebra", "alpha", "mango"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeDomains(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("len: got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d]: got %q want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParsePresetArgs(t *testing.T) {
	cases := []struct {
		in       string
		sub, rst string
	}{
		{"", "", ""},
		{"save", "save", ""},
		{"save gaming", "save", "gaming"},
		{"SAVE gaming steam", "save", "gaming steam"},
		{"delete old", "delete", "old"},
	}
	for _, c := range cases {
		sub, rest := parsePresetArgs(c.in)
		if sub != c.sub || rest != c.rst {
			t.Errorf("parsePresetArgs(%q) = (%q,%q) want (%q,%q)", c.in, sub, rest, c.sub, c.rst)
		}
	}
}
