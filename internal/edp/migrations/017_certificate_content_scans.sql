CREATE TABLE certificate_scan_files (
 id text PRIMARY KEY,
 campaign_id text NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
 scan_id text NOT NULL,
 file_id text NOT NULL,
 filename text NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','processing','done','error')),
 lease_token text NOT NULL DEFAULT '',
 lease_until timestamptz,
 attempts integer NOT NULL DEFAULT 0,
 pages integer NOT NULL DEFAULT 0,
 method text NOT NULL DEFAULT '',
 quality integer NOT NULL DEFAULT 0,
 text_enc text NOT NULL DEFAULT '',
 checksum text NOT NULL DEFAULT '',
 error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 UNIQUE(campaign_id,scan_id,file_id)
);
CREATE INDEX certificate_scan_claim ON certificate_scan_files(state,lease_until,created_at);
INSERT INTO schema_migrations(version) VALUES(17);
