ALTER TABLE platform_settings ADD COLUMN named_relays_only boolean NOT NULL DEFAULT true;
ALTER TABLE platform_settings ADD COLUMN legacy_relay_backup jsonb NOT NULL DEFAULT '{}';
INSERT INTO schema_migrations(version) VALUES(13);
