-- Which libraries an account can see. all_libraries = 1 (every account
-- that existed before sharing, and owners always) sees every library,
-- including ones added later; 0 sees only the libraries in user_libraries.
ALTER TABLE users ADD COLUMN all_libraries INTEGER NOT NULL DEFAULT 1;

CREATE TABLE user_libraries (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
	PRIMARY KEY (user_id, library_id)
);
