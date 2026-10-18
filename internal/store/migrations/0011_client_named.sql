-- 1 when the owner renamed a device in the manager: the name the app
-- reports when it starts no longer replaces it.
ALTER TABLE clients ADD COLUMN named INTEGER NOT NULL DEFAULT 0;
