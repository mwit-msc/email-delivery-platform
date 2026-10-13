ALTER TABLE applications ADD COLUMN from_name text NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN from_name text NOT NULL DEFAULT '';
INSERT INTO schema_migrations(version) VALUES(12);
