package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/amarnathcjd/gogram/telegram"
)

// PresetStore holds per-user named domain presets, in memory.
//
// Persistence note: this bot runs on ephemeral GitHub Actions runners that
// lose their disk when the job ends, so durable storage would be wasted
// effort. Presets live for the duration of the bot process — which is the
// window where a user is actively extracting and wants to reuse a target set
// across multiple archives in the same session.
type PresetStore struct {
	mu sync.Mutex
	// userID -> presetName -> domains (lowercased, deduped)
	data map[int64]map[string][]string
}

var presets = &PresetStore{data: map[int64]map[string][]string{}}

// Save stores a preset for the user. Domains are lowercased, whitespace-split,
// and deduped. Returns the normalized domains. An empty name or empty domain
// list is rejected.
func (p *PresetStore) Save(userID int64, name string, domains []string) ([]string, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, fmt.Errorf("preset needs a name")
	}
	clean := normalizeDomains(domains)
	if len(clean) == 0 {
		return nil, fmt.Errorf("preset needs at least one domain")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	user := p.data[userID]
	if user == nil {
		user = map[string][]string{}
		p.data[userID] = user
	}
	user[name] = clean
	return clean, nil
}

// Get returns the domains for a user's preset, or nil if not found.
func (p *PresetStore) Get(userID int64, name string) []string {
	name = strings.TrimSpace(strings.ToLower(name))
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.data[userID][name]
}

// List returns the user's preset names, sorted alphabetically.
func (p *PresetStore) List(userID int64) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	user := p.data[userID]
	if user == nil {
		return nil
	}
	out := make([]string, 0, len(user))
	for k := range user {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Delete removes a preset. Returns false if it didn't exist.
func (p *PresetStore) Delete(userID int64, name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	p.mu.Lock()
	defer p.mu.Unlock()
	user := p.data[userID]
	if user == nil {
		return false
	}
	if _, ok := user[name]; !ok {
		return false
	}
	delete(user, name)
	return true
}

// normalizeDomains lowercases, splits on whitespace/commas/semicolons, trims,
// drops empties, and dedupes while preserving first-seen order.
func normalizeDomains(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range in {
		for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
		}) {
			d := strings.ToLower(strings.TrimSpace(field))
			if d == "" {
				continue
			}
			if _, ok := seen[d]; ok {
				continue
			}
			seen[d] = struct{}{}
			out = append(out, d)
		}
	}
	return out
}

// --- Bot command/reply/callback handlers ---

// parsePresetArgs splits a "/preset ..." argument string into subcommand + rest.
func parsePresetArgs(text string) (sub string, rest string) {
	rest = strings.TrimSpace(text)
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		return strings.ToLower(rest[:i]), strings.TrimSpace(rest[i+1:])
	}
	return strings.ToLower(rest), ""
}

// handlePresetCommand handles /preset, /preset save NAME domains, /preset delete NAME.
func handlePresetCommand(bot *Bot, m *telegram.NewMessage) {
	raw := strings.TrimSpace(strings.TrimPrefix(m.Text(), m.GetCommand()))
	sub, rest := parsePresetArgs(raw)
	userID := m.SenderID()

	switch sub {
	case "", "list", "show":
		showPresetMenuReply(bot, m, userID)
	case "save":
		if rest == "" {
			reply(bot, m, "usage: `/preset save NAME domain1 domain2 …`")
			return
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			reply(bot, m, "usage: `/preset save NAME domain1 domain2 …`")
			return
		}
		name := fields[0]
		domains := normalizeDomains(fields[1:])
		if _, err := presets.Save(userID, name, domains); err != nil {
			reply(bot, m, friendlyError(err))
			return
		}
		reply(bot, m, fmt.Sprintf("✅ preset `*%s*` saved with `%d` domain(s)", escapeMd(name), len(domains)))
	case "delete", "remove", "del":
		if rest == "" {
			reply(bot, m, "usage: `/preset delete NAME`")
			return
		}
		name := strings.ToLower(rest)
		if presets.Delete(userID, name) {
			reply(bot, m, "🗑 preset `*"+escapeMd(name)+"*` deleted")
		} else {
			reply(bot, m, "no preset named `*"+escapeMd(name)+"*`")
		}
	default:
		reply(bot, m, "subcommands: `save`, `delete`, `list`\ne.g. `/preset save gaming steam epicgames riot`")
	}
}

// showPresetMenuReply renders the preset list as an inline-keyboard menu, sent
// as a reply to the user's /preset command. Used outside a session.
func showPresetMenuReply(bot *Bot, m *telegram.NewMessage, userID int64) {
	names := presets.List(userID)
	var b strings.Builder
	b.WriteString("⭐ *presets*\n")
	if len(names) == 0 {
		b.WriteString("\nno saved presets yet\nsave one: `/preset save gaming steam epicgames`")
	} else {
		b.WriteString(fmt.Sprintf("\n`%d` preset(s):\n", len(names)))
		for _, n := range names {
			doms := presets.Get(userID, n)
			b.WriteString(fmt.Sprintf("• *%s* — `%d` domain(s)\n", escapeMd(n), len(doms)))
		}
		b.WriteString("\ntap a preset during domain selection to apply it")
	}
	reply(bot, m, b.String())
}

// showPresetMenuSession renders the preset picker as an inline keyboard tied to
// the session — shown when the user taps "⭐ presets" on the domain selector.
func showPresetMenuSession(bot *Bot, s *Session) {
	names := presets.List(s.UserID)
	var b strings.Builder
	b.WriteString("⭐ *presets*\n")
	if len(names) == 0 {
		b.WriteString("\nno saved presets\nsave one with `/preset save NAME domains…`")
	} else {
		b.WriteString(fmt.Sprintf("\n`%d` preset(s) — tap to apply:\n", len(names)))
		for _, n := range names {
			doms := presets.Get(s.UserID, n)
			b.WriteString(fmt.Sprintf("• *%s* — `%d` domain(s)\n", escapeMd(n), len(doms)))
		}
	}

	var rows [][]telegram.KeyboardButton
	for _, n := range names {
		rows = append(rows, []telegram.KeyboardButton{
			cbBtn(fmt.Sprintf("⭐ %s", truncate(n, 22)), "pa:"+s.ID+":"+n),
			cbBtn("🗑", "pd:"+s.ID+":"+n),
		})
	}
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn("💾 save current", "ps:"+s.ID),
		cbBtn("➕ new from text", "pc:"+s.ID),
	})
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn("◀️ back", "pb:"+s.ID),
	})

	if s.SelectorMsgID != 0 {
		bot.EditTextWithKeyboard(s.ChatID, s.SelectorMsgID, b.String(), inlineKeyboard(rows...))
	} else {
		sent, _ := bot.SendTextWithKeyboard(s.ChatID, b.String(), inlineKeyboard(rows...))
		s.mu.Lock()
		s.SelectorMsgID = int(sent.ID)
		s.mu.Unlock()
	}
}

// handlePresetSaveNameReply: user typed a name during "save current selection"
// flow from the selector's presets button.
func handlePresetSaveNameReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	name := strings.TrimSpace(m.Text())
	bot.DeleteMessage(m.ChatID(), int(m.ID))
	if name == "" {
		bot.SendText(s.ChatID, "preset save cancelled")
		restoreSelector(bot, s)
		return
	}
	doms := collectSelectedDomains(s)
	if len(doms) == 0 {
		bot.SendText(s.ChatID, "no domains selected — select some first, then save a preset")
		restoreSelector(bot, s)
		return
	}
	clean, err := presets.Save(s.UserID, name, doms)
	if err != nil {
		bot.SendText(s.ChatID, friendlyError(err))
		restoreSelector(bot, s)
		return
	}
	bot.SendText(s.ChatID, fmt.Sprintf(
		"✅ preset `*%s*` saved with `%d` domain(s)", escapeMd(strings.ToLower(name)), len(clean)))
	restoreSelector(bot, s)
}

// handlePresetCreateReply: user typed "name domains..." during the
// "new from text" flow.
func handlePresetCreateReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	text := strings.TrimSpace(m.Text())
	bot.DeleteMessage(m.ChatID(), int(m.ID))
	fields := strings.Fields(text)
	if len(fields) < 2 {
		bot.SendText(s.ChatID, "usage: `NAME domain1 domain2 …`")
		restoreSelector(bot, s)
		return
	}
	name := fields[0]
	clean, err := presets.Save(s.UserID, name, normalizeDomains(fields[1:]))
	if err != nil {
		bot.SendText(s.ChatID, friendlyError(err))
		restoreSelector(bot, s)
		return
	}
	bot.SendText(s.ChatID, fmt.Sprintf(
		"✅ preset `*%s*` saved with `%d` domain(s)", escapeMd(strings.ToLower(name)), len(clean)))
	restoreSelector(bot, s)
}

// restoreSelector puts the session back into selecting state and redraws the
// keyboard — used after a preset flow that didn't consume the session.
func restoreSelector(bot *Bot, s *Session) {
	s.mu.Lock()
	s.State = StateSelecting
	s.mu.Unlock()
	redrawSelector(bot, s)
}

// collectSelectedDomains returns the currently-selected exact domains plus
// custom substrings, all lowercased — the snapshot to save as a preset.
func collectSelectedDomains(s *Session) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for d, sel := range s.Selected {
		if sel {
			out = append(out, d)
		}
	}
	out = append(out, s.CustomDomains...)
	return out
}

// applyPreset merges a preset's domains into the session as custom substrings
// (so it matches regardless of TLD), then re-runs extraction.
func applyPreset(bot *Bot, s *Session, name string) {
	doms := presets.Get(s.UserID, name)
	if len(doms) == 0 {
		bot.SendText(s.ChatID, "preset `*"+escapeMd(name)+"*` is empty or missing")
		return
	}
	s.mu.Lock()
	for _, d := range doms {
		dup := false
		for _, ex := range s.CustomDomains {
			if ex == d {
				dup = true
				break
			}
		}
		if !dup {
			s.CustomDomains = append(s.CustomDomains, d)
		}
	}
	s.mu.Unlock()
	generateAndSend(bot, s)
}
