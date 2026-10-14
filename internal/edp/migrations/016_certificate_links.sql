ALTER TABLE campaigns ADD COLUMN certificate_links jsonb NOT NULL DEFAULT '{}';
INSERT INTO schema_migrations(version) VALUES (16);
