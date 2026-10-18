package api

import (
	"context"
	"sort"

	"kinora/internal/store"
)

// ItemView is an item as the apps see it: metadata plus artwork URLs and
// the signed-in user's watch state.
type ItemView struct {
	store.Item
	PosterURL   string            `json:"posterUrl"`
	BackdropURL string            `json:"backdropUrl"`
	Watch       *store.WatchState `json:"watch,omitempty"`
}

// VersionView is a playable file with its tracks and stream URL.
type VersionView struct {
	store.Version
	Streams   []store.Stream `json:"streams"`
	StreamURL string         `json:"streamUrl"`
	// TranscodeURL converts the file for players that can't play it as is.
	TranscodeURL string `json:"transcodeUrl,omitempty"`
	// TrickplayURL lists preview thumbnails, when they've been made.
	TrickplayURL string `json:"trickplayUrl,omitempty"`
	// Intro is where the opening titles are, when they were found.
	Intro *store.Intro `json:"intro,omitempty"`
	// EndCredits is where the end credits are, when they were found:
	// players offer the next episode from their start.
	EndCredits *store.EndCredits `json:"endCredits,omitempty"`
}

func imageURL(id, kind string) string { return "/api/v1/images/" + id + "/" + kind }

func viewOf(it store.Item, w *store.WatchState) ItemView {
	v := ItemView{Item: it, Watch: w}
	switch it.Kind {
	case store.ItemEpisode:
		v.PosterURL = imageURL(it.ID, "still")
	default:
		v.PosterURL = imageURL(it.ID, "poster")
	}
	v.BackdropURL = imageURL(it.ID, "backdrop")
	return v
}

// views converts items, attaching watch state in one query.
func (s *Server) views(ctx context.Context, profileID int64, items []store.Item) ([]ItemView, error) {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	states, err := s.Store.WatchStates(ctx, profileID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ItemView, len(items))
	for i, it := range items {
		var w *store.WatchState
		if st, ok := states[it.ID]; ok {
			w = &st
		}
		out[i] = viewOf(it, w)
	}
	return out, nil
}

func (s *Server) versionViews(ctx context.Context, itemID string) ([]VersionView, error) {
	versions, err := s.Store.VersionsForItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]VersionView, 0, len(versions))
	for _, v := range versions {
		streams, err := s.Store.Streams(ctx, v.ID)
		if err != nil {
			return nil, err
		}
		vv := VersionView{Version: v, Streams: streams, StreamURL: "/api/v1/stream/" + v.ID}
		if s.Transcoder != nil {
			vv.TranscodeURL = "/api/v1/transcode/" + v.ID
		}
		if s.Trickplay != nil {
			if _, ok := s.Trickplay.Ready(v); ok {
				vv.TrickplayURL = "/api/v1/trickplay/" + v.ID
			}
		}
		if in, err := s.Store.IntroFor(ctx, v.ID); err == nil {
			vv.Intro = in
		}
		if c, err := s.Store.EndCreditsFor(ctx, v.ID); err == nil {
			vv.EndCredits = c
		}
		out = append(out, vv)
	}
	return out, nil
}

// recentlyAdded merges newest movies and shows across libraries.
func (s *Server) recentlyAdded(ctx context.Context, limit int) ([]store.Item, error) {
	var all []store.Item
	for _, kind := range []string{store.ItemMovie, store.ItemShow} {
		items, err := s.Store.ListItems(ctx, store.ItemFilter{Kind: kind, Sort: "added", Limit: limit})
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].AddedAt > all[j].AddedAt })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
