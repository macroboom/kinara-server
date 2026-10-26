package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// Editing a title by hand (owners): its name, year, description, genres
// and artwork. Whatever is changed is locked, so matching and refreshing
// from TMDB leave it as edited (see store/edit.go); unlocking a field
// fetches TMDB's again.

func (s *Server) editRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/items/{id}/edit", s.admin(s.getEdit))
	mux.Handle("PATCH /api/v1/items/{id}", s.admin(s.patchItem))
	mux.Handle("POST /api/v1/items/{id}/unlock", s.admin(s.unlockItem))
	mux.Handle("GET /api/v1/items/{id}/artwork/{kind}", s.admin(s.artworkChoices))
	mux.Handle("PUT /api/v1/items/{id}/images/{kind}", s.admin(s.pickArtwork))
	mux.Handle("POST /api/v1/items/{id}/images/{kind}/upload", s.admin(s.uploadArtwork))
}

// editView is what the edit dialog shows.
type editView struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	SortTitle string   `json:"sortTitle"`
	Year      int      `json:"year"`
	Overview  string   `json:"overview"`
	Rating    float64  `json:"rating"`
	AirDate   string   `json:"airDate"`
	Genres    []string `json:"genres"`
	// Locked are the fields edited by hand.
	Locked []string `json:"locked"`
	// Art are the kinds of artwork this kind of title has.
	Art []string `json:"art"`
	// Images are the current artwork addresses (empty when there's none).
	Images map[string]string `json:"images"`
	// CanPick says TMDB artwork can be listed (a TMDB match and a key).
	CanPick bool `json:"canPick"`
}

// artKinds are the artwork kinds a title has.
func artKinds(kind string) []string {
	switch kind {
	case store.ItemEpisode:
		return []string{"still"}
	case store.ItemSeason:
		return []string{"poster"}
	}
	return []string{"poster", "backdrop"}
}

func (s *Server) editViewOf(ctx context.Context, id string) (editView, error) {
	it, err := s.Store.GetItem(ctx, id)
	if err != nil {
		return editView{}, err
	}
	locks, err := s.Store.ItemLocks(ctx, id)
	if err != nil {
		return editView{}, err
	}
	genres, _ := s.Store.ItemGenres(ctx, id)
	v := editView{ID: it.ID, Kind: it.Kind, Title: it.Title, SortTitle: it.SortTitle, Year: it.Year, Overview: it.Overview,
		Rating: it.Rating, AirDate: it.AirDate, Genres: genres, Locked: locks, Art: artKinds(it.Kind), Images: map[string]string{}}
	for _, k := range v.Art {
		if u, _ := s.Store.ImageURL(ctx, id, k); u != "" {
			v.Images[k] = imageURL(id, k) + "~" + strconv.FormatInt(it.UpdatedAt, 36)
		}
	}
	if s.TMDB != nil {
		_, _, err := s.tmdbTarget(ctx, it)
		v.CanPick = err == nil
	}
	return v, nil
}

func (s *Server) getEdit(w http.ResponseWriter, r *http.Request, u store.User) {
	v, err := s.editViewOf(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "title not found")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) patchItem(w http.ResponseWriter, r *http.Request, u store.User) {
	id := r.PathValue("id")
	if _, err := s.Store.GetItem(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "title not found")
		return
	}
	var e store.ItemEdit
	if err := readJSON(r, &e); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case e.Title != nil && strings.TrimSpace(*e.Title) == "":
		writeErr(w, http.StatusBadRequest, "a title can't be empty")
		return
	case e.Title != nil && len(*e.Title) > 300, e.SortTitle != nil && len(*e.SortTitle) > 300:
		writeErr(w, http.StatusBadRequest, "that title is too long")
		return
	case e.Year != nil && (*e.Year < 0 || *e.Year > 3000):
		writeErr(w, http.StatusBadRequest, "that's not a year")
		return
	case e.Rating != nil && (*e.Rating < 0 || *e.Rating > 10):
		writeErr(w, http.StatusBadRequest, "a rating is from 0 to 10")
		return
	case e.Overview != nil && len(*e.Overview) > 10000:
		writeErr(w, http.StatusBadRequest, "that description is too long")
		return
	case e.AirDate != nil && *e.AirDate != "" && !validDate(*e.AirDate):
		writeErr(w, http.StatusBadRequest, "dates are written YYYY-MM-DD")
		return
	case e.Genres != nil && len(*e.Genres) > 20:
		writeErr(w, http.StatusBadRequest, "that's a lot of genres")
		return
	}
	if err := s.Store.EditItem(r.Context(), id, e); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Log.Info("title edited", "by", u.Username, "item", id)
	s.libraryChanged()
	s.getEdit(w, r, u)
}

func validDate(d string) bool {
	_, err := time.Parse("2006-01-02", d)
	return err == nil
}

// unlockItem: {"fields": ["title", ...] or ["all"]}. The fields go back to
// TMDB's, fetched now when the title is matched.
func (s *Server) unlockItem(w http.ResponseWriter, r *http.Request, u store.User) {
	id := r.PathValue("id")
	var req struct {
		Fields []string `json:"fields"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Fields) == 0 {
		writeErr(w, http.StatusBadRequest, `fields to unlock, or ["all"], are needed`)
		return
	}
	if err := s.Store.Unlock(r.Context(), id, req.Fields); err != nil {
		writeErr(w, http.StatusNotFound, "title not found")
		return
	}
	// Refresh from TMDB: the movie or show the title belongs to is matched
	// again by its known ID, which rewrites everything not locked.
	if s.Matcher != nil {
		if top, err := s.topOf(r.Context(), id); err == nil && top.MatchState != store.MatchUnmatched && top.MatchState != store.MatchManual {
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			if _, err := s.Matcher.MatchItem(ctx, top); err != nil {
				s.Log.Warn("refresh after unlocking", "item", id, "err", err)
			}
			cancel()
		}
	}
	// Uploaded pictures no longer in use go.
	for _, k := range artKinds(kindOf(r.Context(), s.Store, id)) {
		if cur, _ := s.Store.ImageURL(r.Context(), id, k); !strings.HasPrefix(cur, dataPrefix+"artwork/") {
			s.removeUploaded(id, k, "")
		}
	}
	s.libraryChanged()
	s.getEdit(w, r, u)
}

func kindOf(ctx context.Context, st *store.Store, id string) string {
	it, _ := st.GetItem(ctx, id)
	return it.Kind
}

// topOf is the movie or show an item belongs to (itself for those).
func (s *Server) topOf(ctx context.Context, id string) (store.Item, error) {
	it, err := s.Store.GetItem(ctx, id)
	for i := 0; err == nil && i < 2 && (it.Kind == store.ItemSeason || it.Kind == store.ItemEpisode) && it.ParentID != ""; i++ {
		it, err = s.Store.GetItem(ctx, it.ParentID)
	}
	return it, err
}

// tmdbTarget says which TMDB title (and season, episode) holds an item's
// artwork.
func (s *Server) tmdbTarget(ctx context.Context, it store.Item) (tvOrMovie int, isTV bool, err error) {
	top, err := s.topOf(ctx, it.ID)
	if err != nil {
		return 0, false, err
	}
	provider := store.ProviderTMDBMovie
	if top.Kind == store.ItemShow {
		provider, isTV = store.ProviderTMDBTV, true
	}
	v, _ := s.Store.ExternalID(ctx, top.ID, provider)
	id, _ := strconv.Atoi(v)
	if id <= 0 {
		return 0, isTV, errors.New("this title isn't matched to TMDB, so there's no artwork to pick from; upload a picture instead")
	}
	return id, isTV, nil
}

// artChoice is one picture to pick.
type artChoice struct {
	// Path is TMDB's file path, sent back to pick it.
	Path string `json:"path"`
	// URL shows it (through the server).
	URL      string  `json:"url"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Language string  `json:"language,omitempty"`
	Votes    float64 `json:"votes"`
	Current  bool    `json:"current"`
}

func (s *Server) artworkChoices(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	kind := r.PathValue("kind")
	it, err := s.Store.GetItem(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "title not found")
		return
	}
	if !slices.Contains(artKinds(it.Kind), kind) {
		writeErr(w, http.StatusBadRequest, "a "+it.Kind+" has no "+kind)
		return
	}
	if s.TMDB == nil {
		writeErr(w, http.StatusNotImplemented, "no TMDB key on this server")
		return
	}
	id, isTV, err := s.tmdbTarget(ctx, it)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	var imgs tmdb.Images
	switch {
	case it.Kind == store.ItemEpisode:
		imgs, err = s.TMDB.EpisodeImages(ctx, id, it.SeasonNumber, it.EpisodeNumber)
	case it.Kind == store.ItemSeason:
		imgs, err = s.TMDB.SeasonImages(ctx, id, it.SeasonNumber)
	case isTV:
		imgs, err = s.TMDB.TVImages(ctx, id)
	default:
		imgs, err = s.TMDB.MovieImages(ctx, id)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "TMDB: "+err.Error())
		return
	}
	list := imgs.Posters
	switch kind {
	case "backdrop":
		list = imgs.Backdrops
	case "still":
		list = imgs.Stills
	}
	// Best first: most liked, then biggest.
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].VoteAverage != list[j].VoteAverage {
			return list[i].VoteAverage > list[j].VoteAverage
		}
		return list[i].Width > list[j].Width
	})
	if len(list) > 60 {
		list = list[:60]
	}
	current, _ := s.Store.ImageURL(ctx, it.ID, kind)
	out := make([]artChoice, 0, len(list))
	for _, a := range list {
		file := strings.TrimPrefix(a.FilePath, "/")
		if !tmdbImageFile.MatchString(file) {
			continue
		}
		out = append(out, artChoice{Path: a.FilePath, URL: "/api/v1/tmdb-image/" + file, Width: a.Width, Height: a.Height,
			Language: a.Language, Votes: a.VoteAverage, Current: current == s.TMDB.Image(a.FilePath)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"choices": out})
}

// pickArtwork sets TMDB artwork by its path: {"path": "/abc.jpg"}.
func (s *Server) pickArtwork(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	kind := r.PathValue("kind")
	it, err := s.Store.GetItem(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "title not found")
		return
	}
	if !slices.Contains(artKinds(it.Kind), kind) {
		writeErr(w, http.StatusBadRequest, "a "+it.Kind+" has no "+kind)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil || !tmdbImageFile.MatchString(strings.TrimPrefix(req.Path, "/")) || s.TMDB == nil {
		writeErr(w, http.StatusBadRequest, "path from the artwork list is needed")
		return
	}
	if err := s.Store.SetImageByHand(ctx, it.ID, kind, s.TMDB.Image("/"+strings.TrimPrefix(req.Path, "/"))); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.removeUploaded(it.ID, kind, "")
	s.libraryChanged()
	s.getEdit(w, r, u)
}

// maxArtwork caps an uploaded picture.
const maxArtwork = 20 << 20

// uploadArtwork takes a JPEG or PNG as the request body.
func (s *Server) uploadArtwork(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	kind := r.PathValue("kind")
	it, err := s.Store.GetItem(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "title not found")
		return
	}
	if !slices.Contains(artKinds(it.Kind), kind) {
		writeErr(w, http.StatusBadRequest, "a "+it.Kind+" has no "+kind)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxArtwork))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "pictures up to 20 MB")
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		writeErr(w, http.StatusBadRequest, "a JPEG or PNG picture is needed")
		return
	}
	if cfg.Width < 100 || cfg.Height < 100 || cfg.Width > 10000 || cfg.Height > 10000 {
		writeErr(w, http.StatusBadRequest, "the picture should be between 100 and 10000 pixels on each side")
		return
	}
	dir := filepath.Join(s.DataDir, "artwork")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ext := ".jpg"
	if format == "png" {
		ext = ".png"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d%s", it.ID, kind, time.Now().UnixNano(), ext))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	abs, _ := filepath.Abs(path)
	if err := s.Store.SetImageByHand(ctx, it.ID, kind, dataPrefix+"artwork/"+filepath.Base(path)); err != nil {
		os.Remove(path)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.removeUploaded(it.ID, kind, abs)
	s.libraryChanged()
	s.getEdit(w, r, u)
}

// removeUploaded deletes earlier uploads of an item's artwork kind (all
// but keep).
func (s *Server) removeUploaded(itemID, kind, keep string) {
	found, _ := filepath.Glob(filepath.Join(s.DataDir, "artwork", itemID+"-"+kind+"-*"))
	for _, f := range found {
		if abs, _ := filepath.Abs(f); abs != keep {
			os.Remove(f)
		}
	}
}
