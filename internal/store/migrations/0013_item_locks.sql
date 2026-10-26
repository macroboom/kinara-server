-- Fields the owner edited by hand (",title,overview,poster,"), which
-- matching and refreshing from TMDB leave alone.
ALTER TABLE items ADD COLUMN locked TEXT NOT NULL DEFAULT '';
