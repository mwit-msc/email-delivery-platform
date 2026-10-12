ALTER TABLE campaigns ADD COLUMN kind text NOT NULL DEFAULT 'certificate' CHECK(kind IN('certificate','bulk'));
INSERT INTO schema_migrations(version) VALUES(4);
