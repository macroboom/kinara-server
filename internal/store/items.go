package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"kinora/internal/parse"
)

type scanner interface{ Scan(dest ...any) error }

func scanItem(r scanner) (Item, error) {
	var it Item
	err := r.Scan(&it.ID, &it.LibraryID, &it.Kind, &it.ParentID, &it.Title, &it.SortTitle,
		&it.Year, &it.SeasonNumber, &it.EpisodeNumber, &it.Overview, &it.Rating, &it.RuntimeMin,
		&it.AirDate, &it.MatchState, &it.MatchConfidence, &it.ParsedTitle, &it.ParsedYear,
		&it.AddedAt, &it.UpdatedAt)
	return it, err
}

func scanItems(rows *sql.Rows) ([]Item, error) {
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CreateItem inserts it, filling in ID and timestamps.
func (s *Store) CreateItem(ctx context.Context, it *Item) error {
	if it.ID == "" {
		it.ID = NewID()
	}
	t := now()
	it.AddedAt, it.UpdatedAt = t, t
	if it.MatchState == "" {
		it.MatchState = MatchPending
	}
	_, err := s.db.ExecContext(ctx, qInsertItem, it.ID, it.LibraryID, it.Kind, nullStr(it.ParentID),
		it.Title, it.SortTitle, nullPositive(it.Year), nullNumber(it.SeasonNumber),
		nullNumber(it.EpisodeNumber), it.MatchState, it.ParsedTitle, it.ParsedYear, t, t)
	return err
}

func (s *Store) GetItem(ctx context.Context, id string) (Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, qGetItem, id))
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	return it, err
}

func (s *Store) findID(ctx context.Context, q string, args ...any) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// FindByParsed returns the item a previous scan created for the same parsed
// title and year, or "" if none.
func (s *Store) FindByParsed(ctx context.Context, libraryID int64, kind, parsedTitle string, parsedYear int) (string, error) {
	return s.findID(ctx, qFindByParsed, libraryID, kind, parsedTitle, parsedYear)
}

func (s *Store) FindSeason(ctx context.Context, showID string, season int) (string, error) {
	return s.findID(ctx, qFindSeason, showID, season)
}

func (s *Store) FindEpisode(ctx context.Context, seasonID string, episode int) (string, error) {
	return s.findID(ctx, qFindEpisode, seasonID, episode)
}

// FindByExternalID finds another item of the same kind in the library with
// this external ID, excluding excludeID.
func (s *Store) FindByExternalID(ctx context.Context, libraryID int64, kind, provider, value, excludeID string) (string, error) {
	return s.findID(ctx, qFindByExternal, libraryID, kind, provider, value, excludeID)
}

func (s *Store) Children(ctx context.Context, parentID string) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, qChildren, parentID)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

func (s *Store) PendingMatch(ctx context.Context, libraryID int64) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, qPendingMatch, libraryID)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

func (s *Store) UnmatchedItems(ctx context.Context, libraryID int64) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, qUnmatchedItems, libraryID)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

// UpdateItemMetadata saves title, overview, rating and match fields.
func (s *Store) UpdateItemMetadata(ctx context.Context, it Item) error {
	_, err := s.db.ExecContext(ctx, qUpdateItemMeta, it.Title, it.SortTitle, nullPositive(it.Year),
		it.Overview, nullFloat(it.Rating), nullPositive(it.RuntimeMin), nullStr(it.AirDate),
		it.MatchState, it.MatchConfidence, now(), it.ID)
	return err
}

func (s *Store) SetMatchState(ctx context.Context, id, state string, confidence float64) error {
	_, err := s.db.ExecContext(ctx, qSetMatchState, state, confidence, now(), id)
	return err
}

// ResetMatch puts an item back in the queue for matching.
func (s *Store) ResetMatch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, qResetMatch, now(), id)
	return err
}

func (s *Store) SetExternalID(ctx context.Context, itemID, provider, value string) error {
	_, err := s.db.ExecContext(ctx, qSetExternalID, itemID, provider, value)
	return err
}

func (s *Store) ExternalID(ctx context.Context, itemID, provider string) (string, error) {
	return s.findID(ctx, qGetExternalID, itemID, provider)
}

func (s *Store) SetImage(ctx context.Context, itemID, kind, url string) error {
	_, err := s.db.ExecContext(ctx, qSetImage, itemID, kind, url)
	return err
}

// ImageURL returns the source URL for an item's image, or "" if none.
func (s *Store) ImageURL(ctx context.Context, itemID, kind string) (string, error) {
	return s.findID(ctx, qGetImage, itemID, kind)
}

// ShowEpisodes lists a show's available episodes in season and episode order.
func (s *Store) ShowEpisodes(ctx context.Context, showID string) ([]EpisodeRef, error) {
	rows, err := s.db.QueryContext(ctx, qShowEpisodes, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpisodeRef
	for rows.Next() {
		var e EpisodeRef
		if err := rows.Scan(&e.ID, &e.Season, &e.Episode); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ShowsWithPendingEpisodes lists matched shows that have new, unmatched
// episodes (for example a new season added after the show was matched).
func (s *Store) ShowsWithPendingEpisodes(ctx context.Context, libraryID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, qShowsPendingEps, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResetUnmatched puts every unmatched movie and show back in the queue.
func (s *Store) ResetUnmatched(ctx context.Context, libraryID int64) error {
	_, err := s.db.ExecContext(ctx, qResetUnmatched, now(), libraryID)
	return err
}

// ShowFilePath returns the library-relative path of one of a show's episode
// files, or "" if it has none. Used to match by the file names when the
// folder name doesn't match.
func (s *Store) ShowFilePath(ctx context.Context, showID string) (string, error) {
	return s.findID(ctx, qShowFilePath, showID)
}

// ShowOfEpisode returns the show ID for an episode, or "" if it has none.
func (s *Store) ShowOfEpisode(ctx context.Context, episodeID string) (string, error) {
	return s.findID(ctx, qShowOfEpisode, episodeID)
}

// ListItems browses items with filters, sorting, and paging.
func (s *Store) ListItems(ctx context.Context, f ItemFilter) ([]Item, error) {
	q, args := buildListItems(f)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanItems(rows)
}

func buildListItems(f ItemFilter) (string, []any) {
	where, args := itemWhere(f)
	q := "SELECT " + itemColsI + " FROM items i"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	switch f.Sort {
	case "added":
		q += " ORDER BY i.added_at DESC, i.sort_title"
	case "year":
		q += " ORDER BY COALESCE(i.year, 0) DESC, i.sort_title"
	case "rating":
		q += " ORDER BY COALESCE(i.rating, 0) DESC, i.sort_title COLLATE NOCASE"
	case "length":
		// Shortest first; titles with no known length go last.
		q += " ORDER BY COALESCE(i.runtime_min, 0) = 0, i.runtime_min, i.sort_title COLLATE NOCASE"
	default:
		q += " ORDER BY i.sort_title COLLATE NOCASE, COALESCE(i.year, 0)"
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, max(f.Offset, 0))
	return q, args
}

// Watch-state conditions for library filters. "seen" means finished or begun.
const (
	// The episodes of show i that have a file.
	showEpisodes = `SELECT e.id FROM items sn JOIN items e ON e.parent_id = sn.id
		WHERE sn.parent_id = i.id AND e.kind = 'episode'
		AND EXISTS (SELECT 1 FROM versions v WHERE v.item_id = e.id AND v.missing = 0)`
	movieSeen      = `EXISTS (SELECT 1 FROM watch_state w WHERE w.profile_id = ? AND w.item_id = i.id AND (w.completed = 1 OR w.position_sec > 0))`
	movieStarted   = `EXISTS (SELECT 1 FROM watch_state w WHERE w.profile_id = ? AND w.item_id = i.id AND w.completed = 0 AND w.position_sec > 0)`
	movieFinished  = `EXISTS (SELECT 1 FROM watch_state w WHERE w.profile_id = ? AND w.item_id = i.id AND w.completed = 1)`
	showSeen       = `EXISTS (SELECT 1 FROM watch_state w WHERE w.profile_id = ? AND w.item_id IN (` + showEpisodes + `) AND (w.completed = 1 OR w.position_sec > 0))`
	showUnfinished = `EXISTS (SELECT 1 FROM (` + showEpisodes + `) ep WHERE NOT EXISTS (
		SELECT 1 FROM watch_state w WHERE w.profile_id = ? AND w.item_id = ep.id AND w.completed = 1))`
	bestQuality = `EXISTS (SELECT 1 FROM versions v WHERE v.missing = 0 AND (v.height >= 1800 OR v.width >= 3200 OR v.hdr != '')
		AND (v.item_id = i.id OR v.item_id IN (` + showEpisodes + `)))`
)

// unmatchedSQL is a movie or show the matcher gave up on.
const unmatchedSQL = `(i.kind IN ('movie', 'show') AND i.match_state = 'unmatched')`

// itemWhere builds the WHERE conditions shared by listing and letter offsets.
func itemWhere(f ItemFilter) ([]string, []any) {
	var where []string
	var args []any
	if f.LibraryID != 0 {
		where = append(where, "i.library_id = ?")
		args = append(args, f.LibraryID)
	} else if f.OnlyOther {
		where = append(where, "(i.library_id IN (SELECT id FROM libraries WHERE plain = 1) OR "+unmatchedSQL+")")
	} else if !f.IncludeOther {
		where = append(where, "i.library_id NOT IN (SELECT id FROM libraries WHERE plain = 1)")
	}
	// Titles that didn't match live on the Other tab until they're fixed.
	if !f.OnlyOther && !f.IncludeUnmatched && f.MatchState == "" {
		where = append(where, "NOT "+unmatchedSQL)
	}
	if f.Kind != "" {
		where = append(where, "i.kind = ?")
		args = append(args, f.Kind)
	} else if f.Search != "" {
		where = append(where, "i.kind IN ('movie', 'show')")
	}
	if f.ParentID != "" {
		where = append(where, "i.parent_id = ?")
		args = append(args, f.ParentID)
	}
	if f.MatchState != "" {
		where = append(where, "i.match_state = ?")
		args = append(args, f.MatchState)
	}
	if !f.IncludeMissing {
		where = append(where, availableSQL)
	}
	p := f.ProfileID
	switch f.Watched {
	case "unwatched":
		where = append(where, "((i.kind = 'movie' AND NOT "+movieSeen+") OR (i.kind = 'show' AND NOT "+showSeen+"))")
		args = append(args, p, p)
	case "started":
		where = append(where, "((i.kind = 'movie' AND "+movieStarted+") OR (i.kind = 'show' AND "+showSeen+" AND "+showUnfinished+"))")
		args = append(args, p, p, p)
	case "watched":
		where = append(where, "((i.kind = 'movie' AND "+movieFinished+") OR (i.kind = 'show' AND "+showSeen+" AND NOT "+showUnfinished+"))")
		args = append(args, p, p, p)
	}
	if f.MaxRuntime > 0 {
		where = append(where, "COALESCE(i.runtime_min, 0) > 0 AND i.runtime_min <= ?")
		args = append(args, f.MaxRuntime)
	}
	if f.BestQuality {
		where = append(where, bestQuality)
	}
	if f.Genre != "" {
		where = append(where, "EXISTS (SELECT 1 FROM item_genres g WHERE g.item_id = i.id AND g.genre = ?)")
		args = append(args, f.Genre)
	}
	for _, word := range strings.Fields(f.Search) {
		like := "%" + escapeLike(strings.ToLower(word)) + "%"
		norm := "%" + escapeLike(parse.Normalize(word)) + "%"
		where = append(where, `(LOWER(i.title) LIKE ? ESCAPE '\' OR i.parsed_title LIKE ? ESCAPE '\')`)
		args = append(args, like, norm)
	}
	return where, args
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(s)
}

// LetterOffsets says where each letter starts in the A to Z listing for a
// filter, for the jump bar. Titles starting with a digit or symbol are "#".
func (s *Store) LetterOffsets(ctx context.Context, f ItemFilter) ([]LetterOffset, error) {
	where, args := itemWhere(f)
	q := "SELECT UPPER(SUBSTR(i.sort_title, 1, 1)) AS l, COUNT(*) FROM items i"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " GROUP BY l ORDER BY l"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LetterOffset{}
	offset := 0
	for rows.Next() {
		var l string
		var n int
		if err := rows.Scan(&l, &n); err != nil {
			return nil, err
		}
		switch {
		case len(l) == 1 && l[0] >= 'A' && l[0] <= 'Z':
			out = append(out, LetterOffset{Letter: l, Offset: offset})
		case l < "A" && (len(out) == 0 || out[0].Letter != "#"):
			out = append([]LetterOffset{{Letter: "#", Offset: 0}}, out...)
		}
		offset += n
	}
	return out, rows.Err()
}

// MergeItem moves everything from one item into another of the same kind and
// deletes the first. Used when two folders turn out to be the same title
// (for example a 4K copy and a 1080p copy of one movie).
func (s *Store) MergeItem(ctx context.Context, fromID, intoID string) error {
	if fromID == intoID {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := mergeTx(ctx, tx, fromID, intoID); err != nil {
		return err
	}
	return tx.Commit()
}

func mergeTx(ctx context.Context, tx *sql.Tx, fromID, intoID string) error {
	t := now()
	if _, err := tx.ExecContext(ctx, qMoveVersions, intoID, fromID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, qMergeWatch, intoID, fromID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, qCopyExternalIDs, intoID, fromID); err != nil {
		return err
	}

	// Children: merge seasons and episodes with matching numbers, move the rest.
	rows, err := tx.QueryContext(ctx, qChildren, fromID)
	if err != nil {
		return err
	}
	children, err := scanItems(rows)
	if err != nil {
		return err
	}
	for _, c := range children {
		var q string
		var num int
		switch c.Kind {
		case ItemSeason:
			q, num = qFindSeason, c.SeasonNumber
		case ItemEpisode:
			q, num = qFindEpisode, c.EpisodeNumber
		default:
			continue
		}
		var target string
		err := tx.QueryRowContext(ctx, q, intoID, num).Scan(&target)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, qReparentItem, intoID, t, c.ID); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if err := mergeTx(ctx, tx, c.ID, target); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, qDeleteItem, fromID)
	return err
}

// DeleteEmptyItems removes items left with no files and no watch history,
// for example after a merge. Watched items are kept so history survives a
// file being moved away and back.
func (s *Store) DeleteEmptyItems(ctx context.Context, libraryID int64) error {
	for _, q := range []string{qDeleteEmptyEps, qDeleteEmptySzn, qDeleteEmptyTops} {
		if _, err := s.db.ExecContext(ctx, q, libraryID); err != nil {
			return err
		}
	}
	return nil
}

func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
