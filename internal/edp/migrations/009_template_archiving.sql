ALTER TABLE templates ADD COLUMN archived boolean NOT NULL DEFAULT false;
INSERT INTO schema_migrations(version) VALUES(9);
