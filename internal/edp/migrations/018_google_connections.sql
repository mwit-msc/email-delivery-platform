CREATE TABLE google_connections (
 user_id text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 id text UNIQUE NOT NULL,
 subject text NOT NULL,
 email text NOT NULL,
 config_hash text NOT NULL,
 access_enc text NOT NULL,
 refresh_enc text NOT NULL,
 token_expiry timestamptz NOT NULL,
 connected_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE google_oauth_states (
 state_hash text PRIMARY KEY,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 session_hash text NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,
 verifier_enc text NOT NULL,
 nonce text NOT NULL,
 config_hash text NOT NULL,
 return_to text NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX google_oauth_states_expiry ON google_oauth_states(expires_at);
INSERT INTO schema_migrations(version) VALUES(18);
