ALTER TABLE messages ADD COLUMN envelope_sender text;
INSERT INTO schema_migrations(version) VALUES(6);
