ALTER TABLE messages ADD COLUMN bcc text[] NOT NULL DEFAULT '{}';
ALTER TABLE campaigns ADD COLUMN bcc text[] NOT NULL DEFAULT '{}';
INSERT INTO schema_migrations(version) VALUES(14);
