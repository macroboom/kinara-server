package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kinara/internal/arr"
	"kinara/internal/store"
)

// Saying why a request isn't downloading. A title sent to Radarr or Sonarr
// shows as on its way ("Looking for a release") until its download shows
// up in their queue. When none does, Radarr and Sonarr know why: it isn't
// out yet, nothing was found, a download failed, Prowlarr has no
// indexers. That reason replaces "Looking for a release", in plain words,
// wherever the apps show the title as on its way.

// arrExplainEvery is how often each waiting title is looked at again.
const arrExplainEvery = 10 * time.Minute

// arrJustAdded: this soon after a title was added the first search is
// likely still running.
const arrJustAdded = 5 * time.Minute

type arrReason struct {
	// done: it has its files after all (imported without Kinara seeing the
	// download): the status ends and the library is scanned.
	done bool
	// gone: the service doesn't have it any more: the status goes.
	gone    bool
	message string
}

// arrExplain looks at the titles still "looking for a release" that have
// no download in the queue, and says why.
func (s *Server) arrExplain(ctx context.Context, a store.ArrSettings, queued map[string]bool) {
	list, err := s.Store.Statuses(ctx, true)
	if err != nil {
		return
	}
	noIndexers := s.prowlarrEmpty(a)
	var series []arr.SeriesInfo
	seriesRead := false
	now := time.Now()
	for _, st := range list {
		if st.Source != arrSourceRadarr && st.Source != arrSourceSonarr {
			continue
		}
		upd := statusUpdate{Kind: st.Kind, TMDBID: st.TMDBID, Title: st.Title, Year: st.Year, Season: -1, State: stateQueued}
		if st.State != stateQueued || st.TMDBID <= 0 || st.Ident != statusIdent(searchingUpdate(upd)) || queued[st.Kind+strconv.Itoa(st.TMDBID)] {
			continue
		}
		s.arr.mu.Lock()
		if s.arr.explained == nil {
			s.arr.explained = map[string]time.Time{}
		}
		last := s.arr.explained[st.Ident]
		if now.Sub(last) < arrExplainEvery {
			s.arr.mu.Unlock()
			continue
		}
		s.arr.explained[st.Ident] = now
		s.arr.mu.Unlock()

		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		var why arrReason
		if st.Source == arrSourceRadarr {
			why, err = s.explainMovie(cctx, a.Radarr, st.TMDBID, noIndexers, now)
		} else {
			if !seriesRead {
				series, err = arrClient(arr.Sonarr, a.Sonarr).AllSeries(cctx)
				seriesRead = err == nil
			}
			if seriesRead {
				why, err = s.explainShow(cctx, a.Sonarr, series, st.TMDBID, noIndexers, now)
			}
		}
		cancel()
		if err != nil {
			s.Log.Debug("couldn't say why a request is waiting", "title", st.Title, "err", err)
			continue
		}
		src := arrSource(arr.Radarr)
		if st.Source == arrSourceSonarr {
			src = arrSource(arr.Sonarr)
		}
		switch {
		case why.gone:
			_ = s.Store.DeleteStatusFor(ctx, st.Source, st.Ident)
		case why.done:
			upd.State = stateDone
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			_, _, _ = s.applyStatus(r, src, upd)
		case why.message != "" && why.message != st.Message:
			upd.Message = why.message
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			if _, _, err := s.applyStatus(r, src, upd); err != nil {
				s.Log.Debug("couldn't update a request's reason", "title", st.Title, "err", err)
			}
		}
	}
}

// prowlarrEmpty: Prowlarr is connected and has no indexers turned on, so
// Radarr and Sonarr have nowhere to search.
func (s *Server) prowlarrEmpty(a store.ArrSettings) bool {
	if a.Prowlarr.URL == "" {
		return false
	}
	s.arr.mu.Lock()
	defer s.arr.mu.Unlock()
	return s.arr.lastErr[arr.Prowlarr] == "" && !s.arr.lastRead[arr.Prowlarr].IsZero() && s.arr.indexers == 0
}

func (s *Server) explainMovie(ctx context.Context, srv store.ArrServer, tmdbID int, noIndexers bool, now time.Time) (arrReason, error) {
	c := arrClient(arr.Radarr, srv)
	m, err := c.MovieByTMDB(ctx, tmdbID)
	if err != nil || m == nil {
		return arrReason{gone: err == nil}, err
	}
	var history []arr.HistoryEvent
	if m.IsAvailable && m.Monitored && !m.HasFile {
		history, _ = c.History(ctx, m.ID)
	}
	return movieReason(*m, history, noIndexers, now), nil
}

// movieReason says why a movie in Radarr isn't downloading.
func movieReason(m arr.MovieInfo, history []arr.HistoryEvent, noIndexers bool, now time.Time) arrReason {
	switch {
	case m.HasFile:
		return arrReason{done: true}
	case !m.Monitored:
		return arrReason{message: "Not monitored in Radarr, so it won't be downloaded"}
	case !m.IsAvailable:
		return arrReason{message: unreleasedText(m, now)}
	}
	if len(history) > 0 && history[0].EventType == "downloadFailed" {
		return arrReason{message: "A download failed; Radarr is looking for another"}
	}
	if noIndexers {
		return arrReason{message: "Prowlarr has no indexers, so Radarr has nowhere to search"}
	}
	if now.Sub(m.Added) < arrJustAdded {
		return arrReason{message: "Looking for a release"}
	}
	return arrReason{message: "No release found yet; Radarr keeps checking on its own"}
}

// unreleasedText: what Radarr is waiting for, with the date when it knows it.
func unreleasedText(m arr.MovieInfo, now time.Time) string {
	if strings.EqualFold(m.MinimumAvailability, "inCinemas") || strings.EqualFold(m.MinimumAvailability, "announced") {
		if m.InCinemas.After(now) {
			return "Waiting for it to reach cinemas (" + shortDate(m.InCinemas, now) + ")"
		}
		return "Not released yet"
	}
	// Released: out on digital or disc.
	next := time.Time{}
	label := ""
	for _, d := range []struct {
		t    time.Time
		what string
	}{{m.DigitalRelease, "digital"}, {m.PhysicalRelease, "disc"}} {
		if d.t.After(now) && (next.IsZero() || d.t.Before(next)) {
			next, label = d.t, d.what
		}
	}
	switch {
	case !next.IsZero() && m.InCinemas.Before(now) && !m.InCinemas.IsZero():
		return "In cinemas now; out on " + label + " " + shortDate(next, now)
	case !next.IsZero():
		return "Waiting for its release: out on " + label + " " + shortDate(next, now)
	case m.InCinemas.Before(now) && !m.InCinemas.IsZero():
		return "In cinemas now; waiting for the digital release"
	case m.InCinemas.After(now):
		return "Not out yet: in cinemas " + shortDate(m.InCinemas, now)
	}
	return "Waiting for its release"
}

func (s *Server) explainShow(ctx context.Context, srv store.ArrServer, series []arr.SeriesInfo, tmdbID int, noIndexers bool, now time.Time) (arrReason, error) {
	var sh *arr.SeriesInfo
	for i := range series {
		if series[i].TMDBID == tmdbID {
			sh = &series[i]
			break
		}
	}
	if sh == nil {
		return arrReason{gone: true}, nil
	}
	c := arrClient(arr.Sonarr, srv)
	eps, err := c.Episodes(ctx, sh.ID)
	if err != nil {
		return arrReason{}, err
	}
	history, _ := c.History(ctx, sh.ID)
	return showReason(*sh, eps, history, noIndexers, now), nil
}

// showReason says why a show in Sonarr isn't downloading.
func showReason(sh arr.SeriesInfo, eps []arr.EpisodeInfo, history []arr.HistoryEvent, noIndexers bool, now time.Time) arrReason {
	if !sh.Monitored {
		return arrReason{message: "Not monitored in Sonarr, so it won't be downloaded"}
	}
	missing, have := 0, 0
	var next time.Time
	for _, e := range eps {
		if !e.Monitored || e.SeasonNumber == 0 {
			continue
		}
		switch {
		case e.HasFile:
			have++
		case e.AirDateUTC.IsZero() || e.AirDateUTC.After(now):
			if !e.AirDateUTC.IsZero() && (next.IsZero() || e.AirDateUTC.Before(next)) {
				next = e.AirDateUTC
			}
		default:
			missing++
		}
	}
	nextText := ""
	if !next.IsZero() {
		nextText = "next episode airs " + shortDate(next, now)
	}
	switch {
	case missing == 0 && have > 0 && next.IsZero():
		return arrReason{done: true}
	case missing == 0 && !next.IsZero():
		return arrReason{message: "Waiting for the next episode: airs " + shortDate(next, now)}
	case missing == 0:
		return arrReason{message: "Nothing aired yet to download"}
	}
	eps1 := fmt.Sprintf("%d episodes", missing)
	if missing == 1 {
		eps1 = "1 episode"
	}
	var msg string
	switch {
	case len(history) > 0 && history[0].EventType == "downloadFailed":
		msg = "A download failed; Sonarr is looking for another"
	case noIndexers:
		msg = "Prowlarr has no indexers, so Sonarr has nowhere to search"
	case now.Sub(sh.Added) < arrJustAdded:
		msg = "Looking for " + eps1
	default:
		msg = "No release found yet for " + eps1 + "; Sonarr keeps checking"
	}
	if nextText != "" {
		msg += "; " + nextText
	}
	return arrReason{message: msg}
}

// shortDate is "Jan 12", or "Jan 12, 2027" in another year.
func shortDate(t, now time.Time) string {
	t = t.Local()
	if t.Year() != now.Year() {
		return t.Format("Jan 2, 2006")
	}
	return t.Format("Jan 2")
}
