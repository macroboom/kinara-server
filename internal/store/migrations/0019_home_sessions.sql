-- A session made on the home network without a password (POST
-- /api/v1/auth/home): it watches, but only from the home network, and it
-- can't manage the server or approve other devices.
ALTER TABLE sessions ADD COLUMN home INTEGER NOT NULL DEFAULT 0;
