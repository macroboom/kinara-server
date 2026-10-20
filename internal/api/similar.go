package api

import (
	"context"
	"net/http"
	"strconv"

	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// "More like this" under a title: TMDB's recommendations for it, each
// marked like a Discover card (in the library, requested, on its way), so
// titles that aren't on the server can be opened and asked for.

// externalSimilar answers GET /api/v1/tmdb/{kind}/{id}/similar?page=N.
func (s *Server) externalSimilar(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.needTMDB(w) {
		return
	}
	kind := r.PathValue("kind")
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 || (kind != "movie" && kind != "show") {
		writeErr(w, http.StatusBadRequest, "expected /tmdb/movie/{id}/similar or /tmdb/show/{id}/similar")
		return
	}
	s.similarAnswer(w, r, u, kind, id)
}

// itemSimilar answers GET /api/v1/items/{id}/similar?page=N for a library
// title (an episode or season answers for its show).
func (s *Server) itemSimilar(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.needTMDB(w) {
		return
	}
	ctx := r.Context()
	it, err := s.Store.GetItem(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	top := s.topItem(ctx, it)
	kind, provider := "movie", store.ProviderTMDBMovie
	if top.Kind == store.ItemShow {
		kind, provider = "show", store.ProviderTMDBTV
	} else if top.Kind != store.ItemMovie {
		writeJSON(w, http.StatusOK, pagedTitles{Items: []trendingView{}, Page: 1, TotalPages: 1})
		return
	}
	ext, err := s.Store.ExternalID(ctx, top.ID, provider)
	id, _ := strconv.Atoi(ext)
	if err != nil || id <= 0 {
		// Not matched to TMDB: nothing to go on.
		writeJSON(w, http.StatusOK, pagedTitles{Items: []trendingView{}, Page: 1, TotalPages: 1})
		return
	}
	s.similarAnswer(w, r, u, kind, id)
}

func (s *Server) similarAnswer(w http.ResponseWriter, r *http.Request, u store.User, kind string, id int) {
	mediaType := "movie"
	if kind == "show" {
		mediaType = "tv"
	}
	page := pageFrom(r)
	key := "similar/" + kind + "/" + strconv.Itoa(id) + "/" + strconv.Itoa(page)
	s.pagedAnswer(w, r, u, key, false, func(ctx context.Context) (tmdb.Page, error) {
		return s.TMDB.Recommendations(ctx, mediaType, id, page)
	})
}

// topItem is the movie or show a library item belongs to (itself for those).
func (s *Server) topItem(ctx context.Context, it store.Item) store.Item {
	for i := 0; i < 2 && (it.Kind == store.ItemEpisode || it.Kind == store.ItemSeason); i++ {
		parent, err := s.Store.GetItem(ctx, it.ParentID)
		if err != nil {
			break
		}
		it = parent
	}
	return it
}
