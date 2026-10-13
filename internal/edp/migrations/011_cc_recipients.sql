ALTER TABLE campaigns ADD COLUMN cc text[] NOT NULL DEFAULT '{}';
ALTER TABLE messages ADD COLUMN cc text[] NOT NULL DEFAULT '{}';
INSERT INTO schema_migrations(version) VALUES(11);
