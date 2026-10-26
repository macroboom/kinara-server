package api

import (
	"context"
	"net/http"
	"time"

	"kinara/internal/store"
)

// Franchises (TMDB collections). With the owner's "Group franchises"
// setting on, movie listings asked with ?collections=1 show a franchise's
// movies as one item of kind "collection" (ID "c" plus TMDB's collection
// ID, like "c119"). GET /items/c119 answers with the franchise and its
// movies (in "items", oldest first); /items/c119/children lists just the
// movies. A movie's page says which franchise it's in ("collection").

// collectionInfo is the franchise a movie page links to.
type collectionInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Count is how many of its movies are on the server.
	Count int `json:"count"`
}

// groupFilter turns on grouping for a movie listing when the app asks for
// it (?collections=1) and the owner's setting allows it.
func (s *Server) groupFilter(ctx context.Context, r *http.Request, f *store.ItemFilter) {
	if r.URL.Query().Get("collections") != "1" {
		return
	}
	if st, err := s.Store.GetSettings(ctx); err == nil && st.GroupCollections {
		f.GroupCollections = true
	}
}

// collectionItem is a franchise as a listing's item.
func collectionItem(c store.Collection, movies []store.Item) store.Item {
	it := store.Item{ID: store.CollectionItemID(c.ID), Kind: store.ItemCollection, Title: c.Name, SortTitle: c.SortName,
		Overview: c.Overview, SeasonNumber: -1, EpisodeNumber: -1, MatchState: store.MatchMatched, MatchConfidence: 1}
	for _, m := range movies {
		if it.LibraryID == 0 {
			it.LibraryID = m.LibraryID
		}
		if m.Year > 0 && (it.Year == 0 || m.Year < it.Year) {
			it.Year = m.Year
		}
		it.AddedAt = max(it.AddedAt, m.AddedAt)
		it.UpdatedAt = max(it.UpdatedAt, m.UpdatedAt)
	}
	return it
}

// fillCounts sets how many movies each franchise in a listing has.
func (s *Server) fillCounts(ctx context.Context, views []ItemView) {
	var ids []int
	for _, v := range views {
		if id, ok := store.CollectionID(v.ID); ok && v.Kind == store.ItemCollection {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sizes, err := s.Store.CollectionSizes(ctx, ids)
	if err != nil {
		return
	}
	for i := range views {
		if id, ok := store.CollectionID(views[i].ID); ok && views[i].Kind == store.ItemCollection {
			views[i].Count = sizes[id]
		}
	}
}

// getCollection answers GET /items/c119.
func (s *Server) getCollection(w http.ResponseWriter, r *http.Request, u store.User, id int) {
	ctx := r.Context()
	c, err := s.Store.GetCollection(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	// The description is fetched the first time the franchise is opened.
	if c.DetailsAt == 0 && s.TMDB != nil {
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if d, err := s.TMDB.CollectionDetails(tctx, id); err == nil {
			c.Overview = d.Overview
			_ = s.Store.SetCollectionDetails(ctx, id, d.Overview)
		}
		cancel()
	}
	movies, err := s.Store.CollectionMovies(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views, err := s.views(ctx, u.ProfileID, movies)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	item := viewOf(collectionItem(c, movies), nil)
	item.Count = len(movies)
	writeJSON(w, http.StatusOK, map[string]any{"item": item, "items": views})
}

// collectionChildren answers GET /items/c119/children.
func (s *Server) collectionChildren(w http.ResponseWriter, r *http.Request, u store.User, id int) {
	movies, err := s.Store.CollectionMovies(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views, err := s.views(r.Context(), u.ProfileID, movies)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

// movieCollection is the franchise a movie is in, when two or more of its
// movies are on the server (else nil).
func (s *Server) movieCollection(ctx context.Context, itemID string) *collectionInfo {
	id, err := s.Store.CollectionOf(ctx, itemID)
	if err != nil || id == 0 {
		return nil
	}
	sizes, err := s.Store.CollectionSizes(ctx, []int{id})
	if err != nil || sizes[id] < 2 {
		return nil
	}
	c, err := s.Store.GetCollection(ctx, id)
	if err != nil {
		return nil
	}
	return &collectionInfo{ID: store.CollectionItemID(id), Title: c.Name, Count: sizes[id]}
}

// collectionImage is a franchise's artwork: its own from TMDB, else its
// first movie's.
func (s *Server) collectionImage(ctx context.Context, id int, kind string) string {
	c, err := s.Store.GetCollection(ctx, id)
	if err != nil {
		return ""
	}
	path := c.PosterPath
	if kind == "backdrop" {
		path = c.BackdropPath
	}
	if path != "" && s.TMDB != nil {
		return s.TMDB.Image(path)
	}
	movies, _ := s.Store.CollectionMovies(ctx, id)
	for _, m := range movies {
		if u, _ := s.Store.ImageURL(ctx, m.ID, kind); u != "" {
			return s.imageSrc(u)
		}
	}
	return ""
}
