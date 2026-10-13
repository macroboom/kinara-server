package store

// All SQL lives here so it can be read and checked in one place.

const itemCols = `id, library_id, kind, COALESCE(parent_id, ''), title, sort_title,
	COALESCE(year, 0), COALESCE(season_number, -1), COALESCE(episode_number, -1),
	overview, COALESCE(rating, 0), COALESCE(runtime_min, 0), COALESCE(air_date, ''),
	match_state, COALESCE(match_confidence, 0), parsed_title, parsed_year, added_at, updated_at`

const itemColsI = `i.id, i.library_id, i.kind, COALESCE(i.parent_id, ''), i.title, i.sort_title,
	COALESCE(i.year, 0), COALESCE(i.season_number, -1), COALESCE(i.episode_number, -1),
	i.overview, COALESCE(i.rating, 0), COALESCE(i.runtime_min, 0), COALESCE(i.air_date, ''),
	i.match_state, COALESCE(i.match_confidence, 0), i.parsed_title, i.parsed_year, i.added_at, i.updated_at`

const versionCols = `id, COALESCE(item_id, ''), library_id, path, rel_path, part, COALESCE(episode_end, 0),
	size, mtime, container, duration_sec, bitrate, width, height, video_codec, hdr, dv_profile,
	probe_error, missing, added_at, scanned_at`

// availableSQL is true when an item (aliased i) has at least one file present.
const availableSQL = `(
	(i.kind IN ('movie', 'episode') AND EXISTS (
		SELECT 1 FROM versions v WHERE v.item_id = i.id AND v.missing = 0))
	OR (i.kind = 'season' AND EXISTS (
		SELECT 1 FROM items e JOIN versions v ON v.item_id = e.id
		WHERE e.parent_id = i.id AND v.missing = 0))
	OR (i.kind = 'show' AND EXISTS (
		SELECT 1 FROM items s JOIN items e ON e.parent_id = s.id JOIN versions v ON v.item_id = e.id
		WHERE s.parent_id = i.id AND v.missing = 0))
)`

// Libraries.
const (
	qInsertLibrary     = `INSERT INTO libraries (name, path, kind, created_at) VALUES (?, ?, ?, ?)`
	qListLibraries     = `SELECT id, name, path, kind, created_at, COALESCE(last_scan_at, 0) FROM libraries ORDER BY name`
	qGetLibrary        = `SELECT id, name, path, kind, created_at, COALESCE(last_scan_at, 0) FROM libraries WHERE id = ?`
	qDeleteLibrary     = `DELETE FROM libraries WHERE id = ?`
	qSetLibraryScanned = `UPDATE libraries SET last_scan_at = ? WHERE id = ?`
	qLibraryStats      = `SELECT
		(SELECT COUNT(*) FROM items WHERE library_id = ? AND kind = 'movie'),
		(SELECT COUNT(*) FROM items WHERE library_id = ? AND kind = 'show'),
		(SELECT COUNT(*) FROM items WHERE library_id = ? AND kind = 'episode'),
		(SELECT COUNT(*) FROM items WHERE library_id = ? AND kind IN ('movie', 'show') AND match_state = 'unmatched'),
		(SELECT COUNT(*) FROM items WHERE library_id = ? AND kind IN ('movie', 'show') AND match_state = 'pending'),
		(SELECT COUNT(*) FROM versions WHERE library_id = ? AND missing = 0),
		(SELECT COUNT(*) FROM versions WHERE library_id = ? AND missing = 1),
		(SELECT COUNT(*) FROM versions WHERE library_id = ? AND probe_error != ''),
		(SELECT COUNT(*) FROM versions WHERE library_id = ? AND item_id IS NULL AND missing = 0)`
)

// Items.
const (
	qInsertItem = `INSERT INTO items (id, library_id, kind, parent_id, title, sort_title, year,
		season_number, episode_number, match_state, parsed_title, parsed_year, added_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qGetItem         = `SELECT ` + itemCols + ` FROM items WHERE id = ?`
	qFindByParsed    = `SELECT id FROM items WHERE library_id = ? AND kind = ? AND parsed_title = ? AND parsed_year = ? LIMIT 1`
	qFindSeason      = `SELECT id FROM items WHERE parent_id = ? AND kind = 'season' AND season_number = ? LIMIT 1`
	qFindEpisode     = `SELECT id FROM items WHERE parent_id = ? AND kind = 'episode' AND episode_number = ? LIMIT 1`
	qChildren        = `SELECT ` + itemCols + ` FROM items WHERE parent_id = ? ORDER BY COALESCE(season_number, 0), COALESCE(episode_number, 0), sort_title`
	qPendingMatch    = `SELECT ` + itemCols + ` FROM items WHERE library_id = ? AND kind IN ('movie', 'show') AND match_state = 'pending' ORDER BY added_at`
	qUnmatchedItems  = `SELECT ` + itemCols + ` FROM items WHERE library_id = ? AND kind IN ('movie', 'show') AND match_state = 'unmatched' ORDER BY sort_title`
	qUpdateItemMeta  = `UPDATE items SET title = ?, sort_title = ?, year = ?, overview = ?, rating = ?, runtime_min = ?, air_date = ?, match_state = ?, match_confidence = ?, updated_at = ? WHERE id = ?`
	qSetMatchState   = `UPDATE items SET match_state = ?, match_confidence = ?, updated_at = ? WHERE id = ?`
	qResetMatch      = `UPDATE items SET match_state = 'pending', updated_at = ? WHERE id = ?`
	qDeleteItem      = `DELETE FROM items WHERE id = ?`
	qReparentItem    = `UPDATE items SET parent_id = ?, updated_at = ? WHERE id = ?`
	qSetExternalID   = `INSERT INTO external_ids (item_id, provider, value) VALUES (?, ?, ?) ON CONFLICT (item_id, provider) DO UPDATE SET value = excluded.value`
	qGetExternalID   = `SELECT value FROM external_ids WHERE item_id = ? AND provider = ?`
	qCopyExternalIDs = `INSERT OR IGNORE INTO external_ids (item_id, provider, value) SELECT ?, provider, value FROM external_ids WHERE item_id = ?`
	qFindByExternal  = `SELECT i.id FROM external_ids e JOIN items i ON i.id = e.item_id
		WHERE i.library_id = ? AND i.kind = ? AND e.provider = ? AND e.value = ? AND i.id != ? LIMIT 1`
	qShowEpisodes = `SELECT e.id, s.season_number, e.episode_number
		FROM items s JOIN items e ON e.parent_id = s.id
		WHERE s.parent_id = ? AND s.kind = 'season' AND e.kind = 'episode'
		AND EXISTS (SELECT 1 FROM versions v WHERE v.item_id = e.id AND v.missing = 0)
		ORDER BY s.season_number, e.episode_number`
	qShowOfEpisode = `SELECT COALESCE(s.parent_id, '') FROM items e JOIN items s ON s.id = e.parent_id WHERE e.id = ?`
	qShowFilePath  = `SELECT v.rel_path FROM items s JOIN items e ON e.parent_id = s.id
		JOIN versions v ON v.item_id = e.id
		WHERE s.parent_id = ? AND v.missing = 0 ORDER BY v.rel_path LIMIT 1`
	qShowsPendingEps = `SELECT DISTINCT sh.id FROM items e
		JOIN items s ON s.id = e.parent_id JOIN items sh ON sh.id = s.parent_id
		WHERE e.library_id = ? AND e.kind = 'episode' AND e.match_state = 'pending'
		AND sh.match_state IN ('matched', 'manual')`
	qResetUnmatched = `UPDATE items SET match_state = 'pending', updated_at = ?
		WHERE library_id = ? AND kind IN ('movie', 'show') AND match_state = 'unmatched'`
	qDeleteEmptyEps  = `DELETE FROM items WHERE library_id = ? AND kind = 'episode' AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.item_id = items.id) AND NOT EXISTS (SELECT 1 FROM watch_state w WHERE w.item_id = items.id)`
	qDeleteEmptySzn  = `DELETE FROM items WHERE library_id = ? AND kind = 'season' AND NOT EXISTS (SELECT 1 FROM items c WHERE c.parent_id = items.id)`
	qDeleteEmptyTops = `DELETE FROM items WHERE library_id = ? AND kind IN ('movie', 'show')
		AND NOT EXISTS (SELECT 1 FROM versions v WHERE v.item_id = items.id)
		AND NOT EXISTS (SELECT 1 FROM items c WHERE c.parent_id = items.id)
		AND NOT EXISTS (SELECT 1 FROM watch_state w WHERE w.item_id = items.id)`
)

// Images.
const (
	qSetImage = `INSERT INTO images (item_id, kind, url) VALUES (?, ?, ?) ON CONFLICT (item_id, kind) DO UPDATE SET url = excluded.url`
	qGetImage = `SELECT url FROM images WHERE item_id = ? AND kind = ?`
)

// Versions and streams.
const (
	qGetVersion       = `SELECT ` + versionCols + ` FROM versions WHERE id = ?`
	qGetVersionByPath = `SELECT ` + versionCols + ` FROM versions WHERE path = ?`
	qLibraryVersions  = `SELECT ` + versionCols + ` FROM versions WHERE library_id = ?`
	qVersionsForItem  = `SELECT ` + versionCols + ` FROM versions WHERE item_id = ? AND missing = 0 ORDER BY height DESC, part`
	qInsertVersion    = `INSERT INTO versions (id, item_id, library_id, path, rel_path, part, episode_end, size, mtime,
		container, duration_sec, bitrate, width, height, video_codec, hdr, dv_profile, probe_error, missing, added_at, scanned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`
	qUpdateVersion = `UPDATE versions SET item_id = ?, rel_path = ?, part = ?, episode_end = ?, size = ?, mtime = ?,
		container = ?, duration_sec = ?, bitrate = ?, width = ?, height = ?, video_codec = ?, hdr = ?,
		dv_profile = ?, probe_error = ?, missing = 0, scanned_at = ? WHERE id = ?`
	qTouchVersion      = `UPDATE versions SET scanned_at = ?, missing = 0 WHERE id = ?`
	qMarkMissing       = `UPDATE versions SET missing = 1 WHERE library_id = ? AND scanned_at < ? AND missing = 0`
	qMoveVersions      = `UPDATE versions SET item_id = ? WHERE item_id = ?`
	qSetVersionItem    = `UPDATE versions SET item_id = ?, scanned_at = ? WHERE id = ?`
	qDeleteStreams     = `DELETE FROM streams WHERE version_id = ?`
	qInsertStream      = `INSERT INTO streams (version_id, idx, type, codec, profile, language, title, channels, is_default, is_forced) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qStreamsForVersion = `SELECT idx, type, codec, profile, language, title, channels, is_default, is_forced FROM streams WHERE version_id = ? ORDER BY idx`
	qProbeErrors       = `SELECT ` + versionCols + ` FROM versions WHERE library_id = ? AND probe_error != '' AND missing = 0 ORDER BY rel_path LIMIT ?`
	qUnlinkedVersions  = `SELECT ` + versionCols + ` FROM versions WHERE library_id = ? AND item_id IS NULL AND missing = 0 ORDER BY rel_path LIMIT ?`
)

// Users and sessions.
const (
	qInsertUser     = `INSERT INTO users (username, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?)`
	qGetUserByName  = `SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ?`
	qGetUser        = `SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = ?`
	qCountUsers     = `SELECT COUNT(*) FROM users`
	qSetPassword    = `UPDATE users SET password_hash = ? WHERE id = ?`
	qInsertSession  = `INSERT INTO sessions (token_hash, user_id, device_name, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`
	qGetSessionUser = `SELECT u.id, u.username, u.password_hash, u.is_admin, u.created_at, s.expires_at, s.last_seen_at,
		COALESCE(s.profile_id, (SELECT MIN(p.id) FROM profiles p WHERE p.user_id = u.id), 0)
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`
	qSetSessionProfile = `UPDATE sessions SET profile_id = ? WHERE token_hash = ?`
	qTouchSession      = `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`
	qDeleteSession     = `DELETE FROM sessions WHERE token_hash = ?`
	qDeleteSessions    = `DELETE FROM sessions WHERE user_id = ?`
	qExpireSessions    = `DELETE FROM sessions WHERE expires_at < ?`
)

// Watch state.
const (
	qGetWatch = `SELECT item_id, position_sec, completed, play_count, last_played_at, client_time
		FROM watch_state WHERE profile_id = ? AND item_id = ?`
	qUpsertWatch = `INSERT INTO watch_state (profile_id, item_id, position_sec, completed, play_count, last_played_at, client_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (profile_id, item_id) DO UPDATE SET position_sec = excluded.position_sec,
		completed = excluded.completed, play_count = excluded.play_count,
		last_played_at = excluded.last_played_at, client_time = excluded.client_time`
	qRecentWatch = `SELECT item_id, position_sec, completed, play_count, last_played_at, client_time
		FROM watch_state WHERE profile_id = ? AND last_played_at >= ? ORDER BY last_played_at DESC LIMIT ?`
	qMergeWatch = `INSERT OR IGNORE INTO watch_state (profile_id, item_id, position_sec, completed, play_count, last_played_at, client_time)
		SELECT profile_id, ?, position_sec, completed, play_count, last_played_at, client_time FROM watch_state WHERE item_id = ?`
)

// Profiles.
const (
	profileCols    = `id, user_id, name, color, pin_hash, kids, audio_lang, subtitle_lang, subtitle_mode, created_at`
	qInsertProfile = `INSERT INTO profiles (user_id, name, color, pin_hash, kids, audio_lang, subtitle_lang, subtitle_mode, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	qProfilesForUser = `SELECT ` + profileCols + ` FROM profiles WHERE user_id = ? ORDER BY id`
	qGetProfile      = `SELECT ` + profileCols + ` FROM profiles WHERE id = ?`
	qUpdateProfile   = `UPDATE profiles SET name = ?, color = ?, pin_hash = ?, kids = ?, audio_lang = ?, subtitle_lang = ?,
		subtitle_mode = ? WHERE id = ?`
	qDeleteProfile = `DELETE FROM profiles WHERE id = ?`

	qGetTrackPrefs = `SELECT audio_lang, subtitle_lang, subtitle_mode FROM show_prefs WHERE profile_id = ? AND item_id = ?`
	qPutTrackPrefs = `INSERT INTO show_prefs (profile_id, item_id, audio_lang, subtitle_lang, subtitle_mode, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (profile_id, item_id) DO UPDATE SET audio_lang = excluded.audio_lang,
		subtitle_lang = excluded.subtitle_lang, subtitle_mode = excluded.subtitle_mode, updated_at = excluded.updated_at`
)

// Cast.
const (
	qCredits        = `SELECT ord, name, role, photo_url FROM credits WHERE item_id = ? ORDER BY ord`
	qCreditPhoto    = `SELECT photo_url FROM credits WHERE item_id = ? AND ord = ?`
	qDeleteCredits  = `DELETE FROM credits WHERE item_id = ?`
	qInsertCredit   = `INSERT INTO credits (item_id, ord, name, role, photo_url) VALUES (?, ?, ?, ?, ?)`
	qSetCreditsAt   = `UPDATE items SET credits_at = ? WHERE id = ?`
	qResetCreditsAt = `UPDATE items SET credits_at = 0 WHERE id = ?`
	qItemsNeedCast  = `SELECT i.id, e.value, i.kind FROM items i JOIN external_ids e ON e.item_id = i.id
		WHERE i.library_id = ? AND i.kind IN ('movie', 'show') AND i.credits_at = 0
		AND e.provider = CASE i.kind WHEN 'movie' THEN 'tmdb_movie' ELSE 'tmdb_tv' END
		LIMIT ?`
)
