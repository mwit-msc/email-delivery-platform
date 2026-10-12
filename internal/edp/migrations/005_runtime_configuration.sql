ALTER TABLE platform_settings ADD COLUMN runtime_config jsonb NOT NULL DEFAULT '{}';
INSERT INTO schema_migrations(version) VALUES(5);
