ALTER TABLE campaign_rows ADD COLUMN source_ordinals integer[] NOT NULL DEFAULT '{}';
UPDATE campaign_rows SET source_ordinals=ARRAY[ordinal];
INSERT INTO schema_migrations(version) VALUES(10);
