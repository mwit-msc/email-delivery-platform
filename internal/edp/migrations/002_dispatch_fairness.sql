ALTER TABLE platform_settings ADD COLUMN IF NOT EXISTS dispatch_counter bigint NOT NULL DEFAULT 0;
INSERT INTO schema_migrations(version) VALUES(2);
