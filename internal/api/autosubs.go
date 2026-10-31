package api

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/ffmpeg"
	"kinara/internal/store"
	"kinara/internal/subs"
	"kinara/internal/subsync"
)

// Automatic subtitles: nobody hunts for subtitles. Each profile has a
// subtitle language and mode; the server works out which files need
// subtitles for them and finds them in the background:
//
//   - "always" wants the language for every file that doesn't have it;
//   - "forced" wants only the foreign-parts subtitles, for files in that
//     language;
//   - "off" still wants the language for files whose sound is in another
//     one (a foreign film), so the player can turn them on.
//
// New files are looked at first, then what's being watched, then the rest,
// paced by a daily budget per service (built-in keys are shared by every
// Kinara server, so nobody's backfill may use them up). A file someone
// starts playing jumps the line.
//
// Each candidate is checked against the film's speech before it's kept
// (internal/subsync, adapted from Silo): one that lines up is kept with the
// timing correction found; one that doesn't belongs to another release or
// title and the next is tried. When nothing lines up, nothing is kept and
// the player can still offer "Find subtitles".

const (
	// autoSubsTries is how many candidates are downloaded and checked for one file.
	autoSubsTries = 4
	// autoSubsGap paces the background work between files (a played file doesn't wait).
	autoSubsGap = 20 * time.Second
	// recentDays: files added this lately come first.
	recentDays = 14
	// watchedDays: files of titles watched this lately come next.
	watchedDays = 60
)

// Retry spacing for a file nothing was found for.
var missingBackoff = []time.Duration{6 * time.Hour, 24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

// Priorities in subtitle_wanted.
const (
	prioRest    = 0
	prioWatched = 1
	prioRecent  = 2
	prioPlaying = 3
)

type autoSubsState struct {
	mu      sync.Mutex
	kick    chan struct{}
	playing map[string]time.Time // version → when it was last raised for play
	planMu  sync.Mutex
	// working is the file being looked at now ("" when idle).
	working string
}

// --- Settings --------------------------------------------------------------------------

const (
	keyAutoSubs       = "auto_subtitles"
	keyAutoSubsBudget = "auto_subtitles_budget"
	keySubDLKey       = "subdl_api_key"
	keyJimakuKey      = "jimaku_api_key"
	keyProviderOff    = "subtitles_provider_off_" // + name: "true" turns a service off
)

// DefaultAutoSubsBudget is how many subtitles the server fetches by itself a day.
const DefaultAutoSubsBudget = 40

// providerLimit is a service's daily cap for this server: shared built-in
// keys get a small share, the owner's own keys more.
func providerLimit(name string, own bool, osAllowed int) int {
	switch name {
	case subs.ProviderOpenSubtitles:
		if osAllowed > 0 {
			return osAllowed
		}
		return 5
	case subs.ProviderSubDL:
		if own {
			return 300
		}
		return 40
	case subs.ProviderSubvault:
		return 100
	case subs.ProviderJimaku:
		return 100
	}
	return 20
}

// ProviderView is a subtitle service on the Manage page.
type ProviderView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Detail says what it's good for.
	Detail string `json:"detail"`
	On     bool   `json:"on"`
	// NeedsKey: it works only with an API key; HasKey: one is set (the
	// owner's, or built into Kinara when Builtin).
	NeedsKey bool `json:"needsKey"`
	HasKey   bool `json:"hasKey"`
	Builtin  bool `json:"builtin"`
	// KeyEnd is the last 4 characters of the owner's own key.
	KeyEnd string `json:"keyEnd,omitempty"`
	Today  int    `json:"today"`
	Limit  int    `json:"limit"`
	// KeyURL is where to get a key.
	KeyURL string `json:"keyUrl,omitempty"`
}

func (s *Server) providerViews(ctx context.Context) []ProviderView {
	get := func(k string) string { v, _ := s.Store.Value(ctx, k); return v }
	used, _ := s.Store.SubtitleUse(ctx, today())
	s.subs.mu.Lock()
	osKey := s.subs.client != nil && s.subs.client.APIKey != ""
	osAllowed := s.subs.osAllowed
	builtinSubDL := s.subs.builtinSubDL != ""
	s.subs.mu.Unlock()
	ownSubDL, ownJimaku := get(keySubDLKey), get(keyJimakuKey)
	end := func(k string) string {
		if k == "" {
			return ""
		}
		return "…" + k[max(0, len(k)-4):]
	}
	out := []ProviderView{
		{Name: subs.ProviderOpenSubtitles, Label: "OpenSubtitles", Detail: "The biggest catalogue, matched to your exact file when it can be.",
			NeedsKey: true, HasKey: osKey, Builtin: get(keySubsAPIKey) == "" && osKey, KeyEnd: end(get(keySubsAPIKey)),
			KeyURL: "https://www.opensubtitles.com/en/consumers"},
		{Name: subs.ProviderSubDL, Label: "SubDL", Detail: "Movies and TV in many languages.",
			NeedsKey: true, HasKey: ownSubDL != "" || builtinSubDL, Builtin: ownSubDL == "" && builtinSubDL, KeyEnd: end(ownSubDL),
			KeyURL: "https://subdl.com/panel/api"},
		{Name: subs.ProviderSubvault, Label: "Subvault (Addic7ed)", Detail: "TV episodes, carefully timed. No key needed.", HasKey: true},
		{Name: subs.ProviderJimaku, Label: "Jimaku", Detail: "Japanese subtitles for anime.", NeedsKey: true, HasKey: ownJimaku != "", KeyEnd: end(ownJimaku),
			KeyURL: "https://jimaku.cc/account"},
	}
	for i := range out {
		p := &out[i]
		p.On = get(keyProviderOff+p.Name) != "true"
		p.Today = used[p.Name]
		p.Limit = providerLimit(p.Name, p.HasKey && !p.Builtin, osAllowed)
	}
	return out
}

// autoProviders are the services turned on and set up, in the order
// their results are preferred when nothing else separates them.
func (s *Server) autoProviders(ctx context.Context) []subs.Provider {
	get := func(k string) string { v, _ := s.Store.Value(ctx, k); return v }
	ua := "Kinara v" + strings.TrimSuffix(s.Version, "-dev")
	var out []subs.Provider
	off := func(name string) bool { return get(keyProviderOff+name) == "true" }
	s.subs.mu.Lock()
	osClient := s.subs.client
	builtinSubDL := s.subs.builtinSubDL
	s.subs.mu.Unlock()
	if osClient != nil && osClient.APIKey != "" && !off(subs.ProviderOpenSubtitles) {
		out = append(out, osClient)
	}
	if k := firstNonEmpty(get(keySubDLKey), builtinSubDL); k != "" && !off(subs.ProviderSubDL) {
		out = append(out, &subs.SubDL{APIKey: k, UserAgent: ua, Base: s.subs.subdlBase, DownloadBase: s.subs.subdlDLBase})
	}
	if !off(subs.ProviderSubvault) {
		out = append(out, &subs.Subvault{UserAgent: ua, Base: s.subs.subvaultBase})
	}
	if k := get(keyJimakuKey); k != "" && !off(subs.ProviderJimaku) {
		out = append(out, &subs.Jimaku{APIKey: k, UserAgent: ua, Base: s.subs.jimakuBase})
	}
	return out
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func (s *Server) autoSubsOn(ctx context.Context) bool {
	v, _ := s.Store.Value(ctx, keyAutoSubs)
	return v != "false" && s.Subtitles != nil
}

func (s *Server) autoSubsBudget(ctx context.Context) int {
	v, _ := s.Store.Value(ctx, keyAutoSubsBudget)
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return DefaultAutoSubsBudget
}

func today() string { return time.Now().UTC().Format("2006-01-02") }

// --- Planning: which files want which subtitles --------------------------------------

// langWant is one profile's wish.
type langWant struct {
	lang string
	mode string // off, forced, always
	// set: the profile chose a subtitle language (not just the default).
	set bool
}

// profileWants lists the distinct (language, mode) wishes of every profile.
// A profile with no subtitle language set wants English, as the apps show.
func (s *Server) profileWants(ctx context.Context) ([]langWant, error) {
	profiles, err := s.Store.AllProfiles(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[langWant]bool{}
	var out []langWant
	for _, p := range profiles {
		lang := subs.Language(p.SubtitleLang)
		w := langWant{lang: lang, mode: p.SubtitleMode, set: lang != ""}
		if lang == "" {
			w.lang = "eng"
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out, nil
}

// wantsFor works out a file's wanted subtitles from the profiles' wishes,
// its tracks and the subtitle files it already has.
func wantsFor(wishes []langWant, streams []store.Stream, files []store.SubtitleFile, priority int) []store.SubtitleWant {
	audio := ""
	for _, st := range streams {
		if st.Type == "audio" && (audio == "" || st.Default) {
			audio = subs.Language(st.Language)
			if st.Default {
				break
			}
		}
	}
	has := func(lang string, forced bool) bool {
		for _, st := range streams {
			if st.Type == "subtitle" && subsync.SameLanguage(subs.Language(st.Language), lang) && st.Forced == forced {
				return true
			}
		}
		for _, f := range files {
			if subsync.SameLanguage(f.Language, lang) && f.Forced == forced {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	var out []store.SubtitleWant
	add := func(lang string, forced bool, prio int) {
		k := lang + strconv.FormatBool(forced)
		if seen[k] || has(lang, forced) {
			return
		}
		seen[k] = true
		out = append(out, store.SubtitleWant{Language: lang, Forced: forced, Priority: prio})
	}
	for _, w := range wishes {
		foreign := audio != "" && !subsync.SameLanguage(audio, w.lang)
		switch w.mode {
		case "always":
			add(w.lang, false, priority)
		case "forced":
			// Foreign-parts subtitles only exist for films in the language;
			// a foreign film wants the whole subtitles instead.
			if foreign {
				add(w.lang, false, priority)
			} else if audio != "" && w.set {
				// Only for profiles that picked a language: the default
				// profile would otherwise search for every film in it.
				add(w.lang, true, max(priority-1, prioRest))
			}
		default: // off
			if foreign {
				add(w.lang, false, priority)
			}
		}
	}
	return out
}

// planAutoSubs updates every file's wanted list. Runs at start, hourly
// and after scans.
func (s *Server) planAutoSubs(ctx context.Context) {
	if !s.autoSubsOn(ctx) {
		return
	}
	s.auto.planMu.Lock()
	defer s.auto.planMu.Unlock()
	wishes, err := s.profileWants(ctx)
	if err != nil || len(wishes) == 0 {
		return
	}
	targets, err := s.Store.AutoSubsTargets(ctx)
	if err != nil {
		s.Log.Warn("auto subtitles: files", "err", err)
		return
	}
	tracks, err := s.Store.AllTracks(ctx)
	if err != nil {
		return
	}
	files, err := s.Store.AllSubtitleFiles(ctx)
	if err != nil {
		return
	}
	wanted, err := s.Store.AllWanted(ctx)
	if err != nil {
		return
	}
	watched, _ := s.Store.WatchedSince(ctx, time.Now().Add(-watchedDays*24*time.Hour).Unix())
	shows, _ := s.Store.ShowsOf(ctx, watched)
	recent := time.Now().Add(-recentDays * 24 * time.Hour).Unix()
	changed := 0
	for _, t := range targets {
		prio := prioRest
		switch {
		case t.Version.AddedAt >= recent:
			prio = prioRecent
		case watched[t.Version.ItemID] || (t.ShowID != "" && shows[t.ShowID]):
			prio = prioWatched
		}
		want := wantsFor(wishes, tracks[t.Version.ID], files[t.Version.ID], prio)
		have := wanted[t.Version.ID]
		// Keep a raised priority (someone is playing it) and found ones.
		for i := range want {
			for _, h := range have {
				if h.Language == want[i].Language && h.Forced == want[i].Forced && h.Priority == prioPlaying && h.State == store.WantWanted {
					want[i].Priority = prioPlaying
				}
			}
		}
		if sameWants(want, have) {
			continue
		}
		if err := s.Store.SyncWanted(ctx, t.Version.ID, want); err != nil {
			s.Log.Warn("auto subtitles: plan", "err", err)
			return
		}
		changed++
	}
	if changed > 0 {
		s.Log.Info("auto subtitles planned", "files", len(targets), "changed", changed)
		s.kickAutoSubs()
	}
}

// sameWants: want matches what's stored (languages, forced and priority).
func sameWants(want []store.SubtitleWant, have []store.SubtitleWant) bool {
	if len(want) != len(have) {
		return false
	}
	for _, w := range want {
		found := false
		for _, h := range have {
			if h.Language == w.Language && h.Forced == w.Forced && h.Priority == w.Priority {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// planOne updates one file's wanted list now (a file just played, or a
// subtitle just turned down), raising it to the front when playing.
func (s *Server) planOne(ctx context.Context, versionID string, playing bool) {
	if !s.autoSubsOn(ctx) {
		return
	}
	wishes, err := s.profileWants(ctx)
	if err != nil {
		return
	}
	streams, _ := s.Store.Streams(ctx, versionID)
	files, _ := s.Store.SubtitleFiles(ctx, versionID)
	have, _ := s.Store.WantedFor(ctx, versionID)
	prio := prioRest
	if playing {
		prio = prioPlaying
	} else {
		for _, h := range have {
			prio = max(prio, h.Priority)
		}
	}
	want := wantsFor(wishes, streams, files, prio)
	if !sameWants(want, have) {
		_ = s.Store.SyncWanted(ctx, versionID, want)
	}
	if playing {
		// One looked for not long ago and found nothing isn't asked for again yet.
		have, _ = s.Store.WantedFor(ctx, versionID)
		for _, w := range have {
			if w.State == store.WantFound || (w.State == store.WantMissing && time.Since(time.Unix(w.LastAt, 0)) < time.Hour) {
				continue
			}
			w.Priority, w.NextAt = prioPlaying, 0
			_ = s.Store.UpdateWanted(ctx, w)
		}
		s.kickAutoSubs()
	}
}

// autoSubsPlaying is told when a file starts playing (once in a while per file).
func (s *Server) autoSubsPlaying(versionID string) {
	if versionID == "" || strings.HasPrefix(versionID, optimizedPrefix) {
		return
	}
	s.auto.mu.Lock()
	if s.auto.playing == nil {
		s.auto.playing = map[string]time.Time{}
	}
	if t, ok := s.auto.playing[versionID]; ok && time.Since(t) < 10*time.Minute {
		s.auto.mu.Unlock()
		return
	}
	if len(s.auto.playing) > 1000 {
		s.auto.playing = map[string]time.Time{}
	}
	s.auto.playing[versionID] = time.Now()
	s.auto.mu.Unlock()
	go s.planOne(s.BaseCtx, versionID, true)
}

// --- Finding --------------------------------------------------------------------------

func (s *Server) kickAutoSubs() {
	s.auto.mu.Lock()
	if s.auto.kick == nil {
		s.auto.kick = make(chan struct{}, 1)
	}
	k := s.auto.kick
	s.auto.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// RunAutoSubs plans and finds subtitles in the background until ctx ends.
func (s *Server) RunAutoSubs(ctx context.Context) {
	if s.Subtitles == nil {
		return
	}
	s.kickAutoSubs()
	s.auto.mu.Lock()
	kick := s.auto.kick
	s.auto.mu.Unlock()
	plan := time.NewTicker(time.Hour)
	defer plan.Stop()
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	s.planAutoSubs(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-plan.C:
			s.planAutoSubs(ctx)
		case <-tick.C:
		case <-kick:
		}
		for ctx.Err() == nil {
			w, did := s.autoSubsOne(ctx)
			if !did {
				break
			}
			if w.Priority == prioPlaying {
				continue
			}
			// Background work goes gently; a file someone plays meanwhile
			// is looked at straight away.
			select {
			case <-ctx.Done():
				return
			case <-kick:
			case <-time.After(autoSubsGap):
			}
		}
	}
}

// autoSubsOne looks for the next wanted subtitle (did is false when there
// was nothing to do, or the day's budget is used).
func (s *Server) autoSubsOne(ctx context.Context) (store.SubtitleWant, bool) {
	if !s.autoSubsOn(ctx) {
		return store.SubtitleWant{}, false
	}
	w, err := s.Store.NextWanted(ctx, time.Now().Unix())
	if err != nil {
		return w, false
	}
	used, _ := s.Store.SubtitleUse(ctx, today())
	budget := s.autoSubsBudget(ctx)
	// A file being played may go a little over the day's budget.
	if (w.Priority < prioPlaying && used["auto"] >= budget) || used["auto"] >= budget+budget/2 {
		return w, false
	}
	s.auto.mu.Lock()
	s.auto.working = w.VersionID
	s.auto.mu.Unlock()
	defer func() {
		s.auto.mu.Lock()
		s.auto.working = ""
		s.auto.mu.Unlock()
	}()
	fctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	found, note, retryLater := s.findSubtitle(fctx, w)
	w.LastAt = time.Now().Unix()
	w.Note = note
	switch {
	case found:
		w.State, w.Tries, w.NextAt = store.WantFound, w.Tries+1, 0
	case retryLater:
		// The services were busy or the budget ran out: soon, not a try.
		w.NextAt = time.Now().Add(time.Hour).Unix()
	default:
		w.State = store.WantMissing
		w.NextAt = time.Now().Add(missingBackoff[min(w.Tries, len(missingBackoff)-1)]).Unix()
		w.Tries++
		if w.Priority == prioPlaying {
			w.Priority = prioWatched
		}
	}
	if err := s.Store.UpdateWanted(ctx, w); err != nil {
		s.Log.Warn("auto subtitles: save", "err", err)
	}
	return w, true
}

// candidate is a search result with the service it came from.
type candidate struct {
	subs.Result
	provider subs.Provider
	order    int
	score    float64
}

// findSubtitle searches every service for one wanted subtitle, then
// fetches and checks the best candidates in turn. It says whether one was
// kept, why not, and whether to try again soon (busy services).
func (s *Server) findSubtitle(ctx context.Context, w store.SubtitleWant) (found bool, note string, retryLater bool) {
	v, err := s.Store.GetVersion(ctx, w.VersionID)
	if err != nil || v.Missing {
		return false, "the file is gone", false
	}
	q, err := s.subtitleQuery(ctx, v)
	if err != nil {
		return false, err.Error(), false
	}
	if c2 := subs.Code2(w.Language); c2 != "" {
		q.Languages = []string{c2}
	} else {
		return false, "unknown language " + w.Language, false
	}
	used, _ := s.Store.SubtitleUse(ctx, today())
	views := s.providerViews(ctx)
	limit := map[string]int{}
	for _, p := range views {
		limit[p.Name] = p.Limit
	}
	var providers []subs.Provider
	for _, p := range s.autoProviders(ctx) {
		// Only OpenSubtitles marks foreign-parts subtitles.
		if w.Forced && p.Name() != subs.ProviderOpenSubtitles {
			continue
		}
		if p.Covers(q) && used[p.Name()] < limit[p.Name()] {
			providers = append(providers, p)
		}
	}
	// Searches are paced too: ten for each subtitle the day allows.
	if used["auto:search"] >= 10*max(s.autoSubsBudget(ctx), 1) && w.Priority < prioPlaying {
		return false, "today's searches are used", true
	}
	_, _ = s.Store.CountSubtitleUse(ctx, "auto:search", today())
	if len(providers) == 0 {
		return false, "no subtitle service can be asked now", true
	}

	// Ask every service at once.
	type answer struct {
		i   int
		res []subs.Result
		err error
	}
	ch := make(chan answer, len(providers))
	for i, p := range providers {
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			res, err := p.Search(sctx, q)
			ch <- answer{i, res, err}
		}()
	}
	rejected, _ := s.Store.Rejected(ctx, v.ID)
	var cands []candidate
	busy := 0
	for range providers {
		a := <-ch
		if a.err != nil {
			if errors.Is(a.err, subs.ErrTryLater) {
				busy++
			} else {
				s.Log.Info("auto subtitles: search failed", "service", providers[a.i].Name(), "err", a.err)
			}
		}
		// SubDL counts searches against its key too.
		if providers[a.i].Name() == subs.ProviderSubDL {
			_, _ = s.Store.CountSubtitleUse(ctx, subs.ProviderSubDL, today())
		}
		for _, r := range a.res {
			if r.Key == "" || rejected[r.Provider+"|"+r.Key] || r.AI || r.Machine {
				continue
			}
			if r.Language != "" && !subsync.SameLanguage(r.Language, w.Language) {
				continue
			}
			if r.Forced != w.Forced {
				continue
			}
			cands = append(cands, candidate{Result: r, provider: providers[a.i], order: a.i})
		}
	}
	if len(cands) == 0 {
		if busy > 0 {
			return false, "the subtitle services were busy", true
		}
		return false, "none found", false
	}
	rankCandidates(cands, v.RelPath)

	// The film's speech, read once for all candidates (and kept for later).
	runtime := time.Duration(v.DurationSec * float64(time.Second))
	speech, speechErr := s.speechFor(ctx, v, w.Language)
	tried := 0
	for _, c := range cands {
		if tried >= autoSubsTries {
			break
		}
		if n := used[c.provider.Name()]; n >= limit[c.provider.Name()] {
			continue
		}
		tried++
		body, err := c.provider.Fetch(ctx, c.Result)
		used[c.provider.Name()], _ = s.Store.CountSubtitleUse(ctx, c.provider.Name(), today())
		_, _ = s.Store.CountSubtitleUse(ctx, "auto", today())
		if err != nil {
			s.Log.Info("auto subtitles: fetch failed", "service", c.provider.Name(), "err", err)
			continue
		}
		f, vtt, err := s.saveSubtitle(ctx, v, c.Result, body, 0, true)
		if err != nil {
			s.Log.Info("auto subtitles: unusable file", "service", c.provider.Name(), "err", err)
			_ = s.Store.RejectSubtitle(ctx, v.ID, c.Provider, c.Key, "unusable")
			continue
		}
		state, timing, verdict := s.checkTiming(vtt, speech, speechErr, runtime, w.Forced)
		if verdict == "no_match" {
			s.Log.Info("auto subtitles: didn't line up, trying the next", "file", v.RelPath, "service", c.provider.Name(), "release", c.Release)
			_ = s.Store.RejectSubtitle(ctx, v.ID, c.Provider, c.Key, "didn't line up with the speech")
			s.removeSubtitleFile(ctx, f)
			continue
		}
		f.TimingScale, f.TimingOffsetMS, f.SyncState = timing.Scale, timing.OffsetMS, state
		if err := s.Store.SetSubtitleTiming(ctx, f.ID, timing.Scale, timing.OffsetMS, state); err != nil {
			return false, err.Error(), true
		}
		s.Log.Info("auto subtitles: found", "file", v.RelPath, "language", w.Language, "service", c.provider.Name(),
			"release", c.Release, "sync", state, "offset_ms", timing.OffsetMS, "scale", timing.Scale)
		return true, c.provider.Name() + ": " + c.Release, false
	}
	if tried == 0 {
		return false, "today's downloads from the services are used", true
	}
	return false, fmt.Sprintf("%d tried, none lined up with the speech", tried), false
}

// rankCandidates puts the likeliest first: made for this exact file, then
// by service, then a release name like the file's, then popularity.
func rankCandidates(cands []candidate, relPath string) {
	words := releaseWords(filepath.Base(relPath))
	for i := range cands {
		c := &cands[i]
		if c.HashMatch {
			c.score += 1000
		}
		c.score += 10 * likeness(words, releaseWords(c.Release+" "+c.FileName))
		c.score -= float64(c.order)
		if c.Downloads > 0 {
			c.score += min(float64(c.Downloads)/1000, 5)
		}
		if c.HearingImpaired {
			c.score -= 0.5
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
}

func releaseWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) > 1 {
			out[w] = true
		}
	}
	return out
}

// likeness is the share of the file's release words the candidate has.
func likeness(file, cand map[string]bool) float64 {
	if len(file) == 0 {
		return 0
	}
	n := 0
	for w := range file {
		if cand[w] {
			n++
		}
	}
	return float64(n) / float64(len(file))
}

// checkTiming checks a subtitle against the speech: its sync state, the
// timing to keep, and the verdict ("no_match" means drop it).
func (s *Server) checkTiming(vtt []byte, speech []subsync.SpeechLevels, speechErr error, runtime time.Duration, forced bool) (string, subsync.Timing, string) {
	cues, err := subsync.ParseCues(vtt)
	if err != nil {
		return "unchecked", subsync.Timing{}, "no_match"
	}
	if speechErr != nil {
		// Nothing to compare with (no sound, no ffmpeg): keep it unchecked.
		return "unchecked", subsync.Timing{}, ""
	}
	a, err := subsync.Align(speech, cues)
	if err != nil {
		return "unchecked", subsync.Timing{}, ""
	}
	switch subsync.Decide(a, subsync.Timing{}, runtime) {
	case subsync.StatusSynced:
		return "synced", a.Timing, ""
	case subsync.StatusAlreadySynced:
		return "already", subsync.Timing{}, ""
	}
	if forced {
		// Foreign-parts subtitles have too few lines to line up: keep them.
		return "unchecked", subsync.Timing{}, ""
	}
	return "", subsync.Timing{}, "no_match"
}

// saveSubtitle keeps a fetched subtitle with the file and converts it to
// WebVTT (which must have cues to be worth keeping).
func (s *Server) saveSubtitle(ctx context.Context, v store.Version, r subs.Result, body []byte, by int64, auto bool) (store.SubtitleFile, []byte, error) {
	body = subs.UTF8(body)
	ext, format := ".srt", "subrip"
	head := strings.ToLower(string(bytes.TrimSpace(body[:min(len(body), 512)])))
	switch {
	case strings.HasPrefix(head, "webvtt"):
		ext, format = ".vtt", "webvtt"
	case strings.Contains(head, "[script info]"):
		ext, format = ".ass", "ass"
	}
	lang := subs.Language(r.Language)
	sum := sha1.Sum([]byte(r.Provider + "|" + r.Key))
	name := firstNonEmpty(r.Provider, subs.ProviderOpenSubtitles) + "-" + hex.EncodeToString(sum[:6])
	if lang != "" {
		name += "." + lang
	}
	dir := filepath.Join(s.DataDir, "subtitles", v.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.SubtitleFile{}, nil, err
	}
	path := filepath.Join(dir, name+ext)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return store.SubtitleFile{}, nil, err
	}
	fi, _ := os.Stat(path)
	vtt, err := s.Subtitles.VTTFile(ctx, "auto-"+v.ID+"-"+name, path, fi.ModTime().UnixNano())
	if err != nil {
		_ = os.Remove(path)
		return store.SubtitleFile{}, nil, err
	}
	if _, err := subsync.ParseCues(vtt); err != nil {
		_ = os.Remove(path)
		return store.SubtitleFile{}, nil, err
	}
	title := strings.TrimSpace(r.Release)
	if len(title) > 80 {
		title = title[:80]
	}
	f := store.SubtitleFile{VersionID: v.ID, Path: dataPrefix + filepath.ToSlash(filepath.Join("subtitles", v.ID, name+ext)),
		Language: lang, Title: title, Format: format, Forced: r.Forced, HearingImpaired: r.HearingImpaired,
		Source: store.SubtitleDownload, ProviderID: r.Key, Provider: firstNonEmpty(r.Provider, subs.ProviderOpenSubtitles),
		AddedBy: by, Auto: auto}
	if err := s.Store.AddSubtitleFile(ctx, &f); err != nil {
		_ = os.Remove(path)
		return f, nil, err
	}
	return f, vtt, nil
}

func (s *Server) removeSubtitleFile(ctx context.Context, f store.SubtitleFile) {
	_ = s.Store.DeleteSubtitleFile(ctx, f.ID)
	if f.Source == store.SubtitleDownload {
		_ = os.Remove(s.dataPath(f.Path))
	}
}

// --- Speech -----------------------------------------------------------------------------

// speechFor reads a file's speech levels (about 45 CPU-seconds of ffmpeg
// for a film, at low priority), kept in the cache so later subtitles for
// the same file are checked straight away.
func (s *Server) speechFor(ctx context.Context, v store.Version, language string) ([]subsync.SpeechLevels, error) {
	if s.Subtitles == nil || s.Subtitles.Tool == nil || !s.Subtitles.Tool.Caps().Found {
		return nil, errors.New("ffmpeg isn't available")
	}
	streams, err := s.Store.Streams(ctx, v.ID)
	if err != nil {
		return nil, err
	}
	var tracks []subsync.AudioTrack
	for _, st := range streams {
		if st.Type == "audio" {
			tracks = append(tracks, subsync.AudioTrack{Ordinal: len(tracks), Language: subs.Language(st.Language), Channels: st.Channels, Default: st.Default})
		}
	}
	plan, err := subsync.PlanSpeech(v.DurationSec, tracks, language)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.speechDir())
	key := fmt.Sprintf("%s-%d-%d-%d.json", v.ID, v.Mtime, plan.AudioOrdinal, len(plan.Windows))
	cachePath := filepath.Join(dir, key)
	if b, err := os.ReadFile(cachePath); err == nil {
		var levels []subsync.SpeechLevels
		if json.Unmarshal(b, &levels) == nil && len(levels) > 0 {
			return levels, nil
		}
	}
	start := time.Now()
	levels, err := subsync.Decode(ctx, s.Subtitles.Tool.Path(), v.Path, plan, ffmpeg.StartLow, nil)
	if err != nil {
		return nil, err
	}
	s.Log.Info("auto subtitles: read the speech", "file", v.RelPath, "windows", len(levels), "took", time.Since(start).Round(time.Second))
	if b, err := json.Marshal(levels); err == nil && os.MkdirAll(dir, 0o755) == nil {
		_ = os.WriteFile(cachePath, b, 0o644)
	}
	return levels, nil
}

func (s *Server) speechDir() string {
	if s.CacheDir != "" {
		return filepath.Join(s.CacheDir, "speech")
	}
	return filepath.Join(s.DataDir, "cache", "speech")
}

// syncSubtitleFile checks a subtitle someone downloaded by hand against
// the speech and keeps the timing correction (in the background).
func (s *Server) syncSubtitleFile(versionID string, fileID int64) {
	ctx, cancel := context.WithTimeout(s.BaseCtx, 15*time.Minute)
	defer cancel()
	v, err := s.Store.GetVersion(ctx, versionID)
	if err != nil {
		return
	}
	f, err := s.Store.SubtitleFile(ctx, fileID)
	if err != nil {
		return
	}
	fi, err := os.Stat(s.dataPath(f.Path))
	if err != nil {
		return
	}
	vtt, err := s.Subtitles.VTTFile(ctx, fmt.Sprintf("%s-x%d", v.ID, f.ID), s.dataPath(f.Path), fi.ModTime().Unix())
	if err != nil {
		return
	}
	speech, speechErr := s.speechFor(ctx, v, f.Language)
	state, timing, verdict := s.checkTiming(vtt, speech, speechErr, time.Duration(v.DurationSec*float64(time.Second)), f.Forced)
	if verdict == "no_match" {
		// Picked by hand: kept, but say it didn't line up.
		state = "no_match"
	}
	_ = s.Store.SetSubtitleTiming(ctx, f.ID, timing.Scale, timing.OffsetMS, state)
}

// --- Routes ----------------------------------------------------------------------------

func (s *Server) autoSubsRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/subtitles/{id}/auto", s.user(s.autoSubsFor))
	mux.Handle("POST /api/v1/subtitles/{id}/files/{file}/wrong", s.user(s.subtitleWrong))
	mux.Handle("GET /api/v1/subtitles/auto", s.admin(s.autoSubsStatus))
	mux.Handle("POST /api/v1/subtitles/auto/retry", s.admin(s.autoSubsRetry))
}

// autoSubsFor answers GET /subtitles/{version}/auto: the file's subtitle
// files and what's being looked for. Players ask while playing (it also
// puts the file at the front of the line), and show subtitles as they come.
func (s *Server) autoSubsFor(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	v, err := s.sourceVersion(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	s.autoSubsPlaying(v.ID)
	wanted, _ := s.Store.WantedFor(ctx, v.ID)
	s.auto.mu.Lock()
	working := s.auto.working == v.ID
	s.auto.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"subtitles": s.externalSubtitles(ctx, v.ID, u),
		"wanted":    wanted,
		// Looking now: a player can say "Finding subtitles…".
		"searching": working,
		"enabled":   s.autoSubsOn(ctx),
	})
}

// subtitleWrong answers POST /subtitles/{version}/files/{index}/wrong:
// someone says a downloaded subtitle is wrong. It's never picked for this
// file again, removed (when fetched automatically, or by this account), and
// the next one is looked for straight away.
func (s *Server) subtitleWrong(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	v, err := s.sourceVersion(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	idx, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("file"), ".vtt"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad subtitle")
		return
	}
	id := int64(idx)
	if idx >= ExternalSubtitleBase {
		id = int64(idx - ExternalSubtitleBase)
	}
	f, err := s.Store.SubtitleFile(ctx, id)
	if err != nil || f.VersionID != v.ID {
		writeErr(w, http.StatusNotFound, "subtitle not found")
		return
	}
	if f.Source != store.SubtitleDownload {
		writeErr(w, http.StatusBadRequest, "this subtitle file is next to the video on the server; the owner can replace it")
		return
	}
	provider := firstNonEmpty(f.Provider, subs.ProviderOpenSubtitles)
	_ = s.Store.RejectSubtitle(ctx, v.ID, provider, f.ProviderID, "said to be wrong by "+u.Username)
	if f.Auto || f.AddedBy == u.ID || u.IsAdmin {
		s.removeSubtitleFile(ctx, f)
	}
	s.Log.Info("subtitle said to be wrong", "by", u.Username, "file", v.RelPath, "service", provider)
	// Look for the next straight away.
	s.auto.mu.Lock()
	delete(s.auto.playing, v.ID)
	s.auto.mu.Unlock()
	s.planOne(ctx, v.ID, true)
	writeJSON(w, http.StatusOK, map[string]any{"searching": s.autoSubsOn(ctx)})
}

// autoSubsStatusView is the Manage page's view.
type autoSubsStatusView struct {
	Enabled   bool             `json:"enabled"`
	Budget    int              `json:"budget"`
	Today     int              `json:"today"`
	Providers []ProviderView   `json:"providers"`
	Counts    map[string]int   `json:"counts"`
	Missing   []missingSubView `json:"missing"`
	Profiles  []string         `json:"profiles"`
}

type missingSubView struct {
	store.SubtitleWant
	Title string `json:"title"`
}

func (s *Server) autoSubsStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	used, _ := s.Store.SubtitleUse(ctx, today())
	counts, _ := s.Store.WantedCounts(ctx)
	out := autoSubsStatusView{Enabled: s.autoSubsOn(ctx), Budget: s.autoSubsBudget(ctx), Today: used["auto"],
		Providers: s.providerViews(ctx), Counts: counts, Missing: []missingSubView{}, Profiles: []string{}}
	missing, _ := s.Store.WantedByState(ctx, store.WantMissing, 50)
	for _, m := range missing {
		mv := missingSubView{SubtitleWant: m}
		if v, err := s.Store.GetVersion(ctx, m.VersionID); err == nil {
			mv.Title = s.downloadName(ctx, v.ItemID)
		}
		out.Missing = append(out.Missing, mv)
	}
	wishes, _ := s.profileWants(ctx)
	for _, x := range wishes {
		out.Profiles = append(out.Profiles, x.lang+":"+x.mode)
	}
	writeJSON(w, http.StatusOK, out)
}

// autoSubsRetry puts missing subtitles back in line (all, or one file's).
func (s *Server) autoSubsRetry(w http.ResponseWriter, r *http.Request, u store.User) {
	var in struct {
		VersionID string `json:"versionId"`
	}
	_ = readJSON(r, &in)
	ctx := r.Context()
	missing, _ := s.Store.WantedByState(ctx, store.WantMissing, 100000)
	n := 0
	for _, m := range missing {
		if in.VersionID != "" && m.VersionID != in.VersionID {
			continue
		}
		m.State, m.NextAt = store.WantWanted, 0
		if in.VersionID != "" {
			m.Priority = prioPlaying
		}
		if s.Store.UpdateWanted(ctx, m) == nil {
			n++
		}
	}
	s.kickAutoSubs()
	writeJSON(w, http.StatusOK, map[string]int{"retried": n})
}

// CheckSubtitleAccount learns the owner's OpenSubtitles account's daily
// downloads at start, for its budget.
func (s *Server) CheckSubtitleAccount(ctx context.Context) {
	s.subs.mu.Lock()
	c := s.subs.client
	s.subs.mu.Unlock()
	if c == nil || c.APIKey == "" || c.Username == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if n, err := c.CheckAccount(cctx); err == nil {
		s.subs.mu.Lock()
		s.subs.osAllowed = n
		s.subs.mu.Unlock()
	}
}

// AutoSubsAfterScan plans new files after a scan.
func (s *Server) AutoSubsAfterScan() {
	go s.planAutoSubs(s.BaseCtx)
}

// subtitleTiming is a subtitle file's correction, for serving.
func subtitleTiming(f store.SubtitleFile) subsync.Timing {
	return subsync.Timing{Scale: f.TimingScale, OffsetMS: f.TimingOffsetMS}
}
