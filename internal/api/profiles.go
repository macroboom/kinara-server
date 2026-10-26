package api

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"kinara/internal/auth"
	"kinara/internal/images"
	"kinara/internal/store"
)

// --- Profiles ("who's watching") ---

const maxProfiles = 12

var (
	rePin   = regexp.MustCompile(`^\d{4}$`)
	reColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

func validSubtitleMode(m string) bool { return m == "off" || m == "forced" || m == "always" }

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request, u store.User) {
	ps, err := s.Store.Profiles(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ps == nil {
		ps = []store.Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": ps, "current": u.ProfileID})
}

type profileRequest struct {
	Name  *string `json:"name"`
	Color *string `json:"color"`
	// Pin is a 4-digit PIN; "" removes it.
	Pin          *string `json:"pin"`
	Kids         *bool   `json:"kids"`
	AudioLang    *string `json:"audioLang"`
	SubtitleLang *string `json:"subtitleLang"`
	SubtitleMode *string `json:"subtitleMode"`
}

// apply copies the fields that were sent onto p, checking each.
func (req profileRequest) apply(p *store.Profile) error {
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 30 {
			return errors.New("name must be 1 to 30 characters")
		}
		p.Name = name
	}
	if req.Color != nil {
		if !reColor.MatchString(*req.Color) {
			return errors.New("color must look like #F2B33D")
		}
		p.Color = *req.Color
	}
	if req.Pin != nil {
		switch {
		case *req.Pin == "":
			p.PinHash = ""
		case rePin.MatchString(*req.Pin):
			h, err := auth.HashPIN(*req.Pin)
			if err != nil {
				return err
			}
			p.PinHash = h
		default:
			return errors.New("PIN must be 4 digits")
		}
		p.HasPin = p.PinHash != ""
	}
	if req.Kids != nil {
		p.Kids = *req.Kids
	}
	if req.AudioLang != nil {
		p.AudioLang = strings.ToLower(strings.TrimSpace(*req.AudioLang))
	}
	if req.SubtitleLang != nil {
		p.SubtitleLang = strings.ToLower(strings.TrimSpace(*req.SubtitleLang))
	}
	if req.SubtitleMode != nil {
		if !validSubtitleMode(*req.SubtitleMode) {
			return errors.New("subtitleMode must be off, forced or always")
		}
		p.SubtitleMode = *req.SubtitleMode
	}
	return nil
}

func (s *Server) createProfile(w http.ResponseWriter, r *http.Request, u store.User) {
	var req profileRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == nil {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	existing, err := s.Store.Profiles(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(existing) >= maxProfiles {
		writeErr(w, http.StatusBadRequest, "an account can have up to 12 profiles")
		return
	}
	p := store.Profile{UserID: u.ID, Color: store.DefaultProfileColor, SubtitleMode: "forced"}
	if err := req.apply(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.CreateProfile(r.Context(), &p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ownProfile loads a profile of the signed-in account, or writes an error.
func (s *Server) ownProfile(w http.ResponseWriter, r *http.Request, u store.User) (store.Profile, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad profile id")
		return store.Profile{}, false
	}
	p, err := s.Store.GetProfile(r.Context(), id)
	if err != nil || p.UserID != u.ID {
		writeErr(w, http.StatusNotFound, "profile not found")
		return store.Profile{}, false
	}
	return p, true
}

func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.ownProfile(w, r, u)
	if !ok {
		return
	}
	// A PIN-protected profile can only be changed while it's selected, so a
	// child can't remove a parent's PIN from their own profile.
	if p.HasPin && u.ProfileID != p.ID && !u.IsAdmin {
		writeErr(w, http.StatusForbidden, "switch to this profile to change it")
		return
	}
	var req profileRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.apply(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateProfile(r.Context(), p); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.ownProfile(w, r, u)
	if !ok {
		return
	}
	if p.HasPin && u.ProfileID != p.ID && !u.IsAdmin {
		writeErr(w, http.StatusForbidden, "switch to this profile to remove it")
		return
	}
	all, err := s.Store.Profiles(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(all) <= 1 {
		writeErr(w, http.StatusBadRequest, "an account needs at least one profile")
		return
	}
	if err := s.Store.DeleteProfile(r.Context(), p.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// selectProfile switches this device to a profile, checking its PIN.
func (s *Server) selectProfile(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.ownProfile(w, r, u)
	if !ok {
		return
	}
	var req struct {
		Pin string `json:"pin"`
	}
	_ = readJSON(r, &req)
	if p.HasPin {
		key := "pin:" + strconv.FormatInt(p.ID, 10)
		if err := s.limiter.Allow(key); err != nil {
			writeErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if !auth.CheckPassword(p.PinHash, req.Pin) {
			s.limiter.Fail(key)
			writeErr(w, http.StatusForbidden, "wrong PIN")
			return
		}
		s.limiter.Reset(key)
	}
	if err := s.Store.SetSessionProfile(r.Context(), sessionFrom(r).tokenHash, p.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// --- Track preferences ---

// topLevelID is the show an episode or season belongs to, or the item itself.
func (s *Server) topLevelID(r *http.Request, it store.Item) string {
	for range 2 {
		if it.Kind != store.ItemEpisode && it.Kind != store.ItemSeason {
			break
		}
		parent, err := s.Store.GetItem(r.Context(), it.ParentID)
		if err != nil {
			break
		}
		it = parent
	}
	return it.ID
}

// effectivePrefs merges a profile's defaults with its choices for a title.
func (s *Server) effectivePrefs(r *http.Request, profileID int64, topID string) store.TrackPrefs {
	out := store.TrackPrefs{SubtitleMode: "forced"}
	if p, err := s.Store.GetProfile(r.Context(), profileID); err == nil {
		out = store.TrackPrefs{AudioLang: p.AudioLang, SubtitleLang: p.SubtitleLang, SubtitleMode: p.SubtitleMode}
	}
	if t, err := s.Store.TrackPrefs(r.Context(), profileID, topID); err == nil {
		if t.AudioLang != "" {
			out.AudioLang = t.AudioLang
		}
		if t.SubtitleLang != "" {
			out.SubtitleLang = t.SubtitleLang
		}
		if t.SubtitleMode != "" {
			out.SubtitleMode = t.SubtitleMode
		}
	}
	return out
}

// putPrefs saves audio and subtitle choices for the show or movie an item
// belongs to, so the next episode starts the same way.
func (s *Server) putPrefs(w http.ResponseWriter, r *http.Request, u store.User) {
	it, err := s.Store.GetItem(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	var req store.TrackPrefs
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.SubtitleMode != "" && !validSubtitleMode(req.SubtitleMode) {
		writeErr(w, http.StatusBadRequest, "subtitleMode must be off, forced or always")
		return
	}
	req.AudioLang = strings.ToLower(strings.TrimSpace(req.AudioLang))
	req.SubtitleLang = strings.ToLower(strings.TrimSpace(req.SubtitleLang))
	top := s.topLevelID(r, it)
	if err := s.Store.PutTrackPrefs(r.Context(), u.ProfileID, top, req); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.effectivePrefs(r, u.ProfileID, top))
}

// --- Cast ---

type castView struct {
	store.Credit
	PhotoURL string `json:"photoUrl,omitempty"`
}

func (s *Server) castFor(r *http.Request, topID string) []castView {
	credits, err := s.Store.Credits(r.Context(), topID)
	if err != nil {
		return []castView{}
	}
	out := make([]castView, 0, len(credits))
	for _, c := range credits {
		v := castView{Credit: c}
		if c.PhotoURL != "" {
			v.PhotoURL = "/api/v1/cast/" + topID + "/" + strconv.Itoa(c.Order)
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) castPhoto(w http.ResponseWriter, r *http.Request, u store.User) {
	ord, err := strconv.Atoi(r.PathValue("ord"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad cast number")
		return
	}
	src, err := s.Store.CreditPhoto(r.Context(), r.PathValue("id"), ord)
	if err != nil || src == "" {
		writeErr(w, http.StatusNotFound, "no photo")
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width == 0 {
		width = 160
	}
	path, err := s.Images.Get(r.Context(), src, images.SnapWidth(width))
	if err != nil {
		s.Log.Warn("cast photo", "src", src, "err", err)
		writeErr(w, http.StatusBadGateway, "photo unavailable")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, path)
}

// --- Browsing: filters, letters, search ---

// itemFilterFrom reads the browse filters shared by /items and /items/letters.
func itemFilterFrom(r *http.Request, u store.User) store.ItemFilter {
	q := r.URL.Query()
	f := store.ItemFilter{
		Kind:       q.Get("kind"),
		ParentID:   q.Get("parentId"),
		MatchState: q.Get("matchState"),
		Sort:       q.Get("sort"),
		ProfileID:  u.ProfileID,
		Search:     strings.TrimSpace(q.Get("q")),
	}
	f.LibraryID, _ = strconv.ParseInt(q.Get("libraryId"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	switch w := q.Get("watched"); w {
	case "unwatched", "started", "watched":
		f.Watched = w
	}
	f.MaxRuntime, _ = strconv.Atoi(q.Get("maxRuntime"))
	f.BestQuality = q.Get("quality") == "best"
	f.Genre = strings.TrimSpace(q.Get("genre"))
	// other=only lists every "Other videos" library together.
	f.OnlyOther = q.Get("other") == "only"
	return f
}

func (s *Server) itemLetters(w http.ResponseWriter, r *http.Request, u store.User) {
	f := itemFilterFrom(r, u)
	s.groupFilter(r.Context(), r, &f)
	letters, err := s.Store.LetterOffsets(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"letters": letters})
}

// search finds movies and shows by title. Titles that start with the
// query come first.
func (s *Server) search(w http.ResponseWriter, r *http.Request, u store.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"items": []ItemView{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	items, err := s.Store.ListItems(r.Context(), store.ItemFilter{Search: q, Limit: limit, ProfileID: u.ProfileID, IncludeOther: true, IncludeUnmatched: true})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lq := strings.ToLower(q)
	rank := func(it store.Item) int {
		t := strings.ToLower(it.Title)
		switch {
		case t == lq:
			return 0
		case strings.HasPrefix(t, lq):
			return 1
		case strings.Contains(t, lq):
			return 2
		}
		return 3
	}
	sort.SliceStable(items, func(i, j int) bool { return rank(items[i]) < rank(items[j]) })
	views, err := s.views(r.Context(), u.ProfileID, items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}
