ALTER TABLE applications ADD COLUMN fallback_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE messages ADD COLUMN route text NOT NULL DEFAULT 'primary' CHECK(route IN('primary','secondary'));
ALTER TABLE messages ADD COLUMN route_attempts integer NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN secondary_used boolean NOT NULL DEFAULT false;
ALTER TABLE messages ADD COLUMN gateway_status text NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN failure_category text NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN pmg_checked_at timestamptz;
CREATE INDEX messages_pmg_pending ON messages(pmg_checked_at,updated_at) WHERE route='primary' AND status IN('relay_accepted','unknown');
INSERT INTO schema_migrations(version) VALUES(3);
