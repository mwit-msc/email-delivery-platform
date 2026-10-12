CREATE TABLE smtp_relays(id text PRIMARY KEY, name text NOT NULL, address text NOT NULL, username text NOT NULL DEFAULT '', password text NOT NULL DEFAULT '', tls text NOT NULL, bounce_domain text NOT NULL DEFAULT '', allowed_domains text[] NOT NULL DEFAULT '{}', enabled boolean NOT NULL DEFAULT true, fallback_id text REFERENCES smtp_relays(id), created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE sender_domain_routes(domain text PRIMARY KEY, relay_id text NOT NULL REFERENCES smtp_relays(id));
ALTER TABLE platform_settings ADD COLUMN strict_sender_domains boolean NOT NULL DEFAULT false;
ALTER TABLE messages ADD COLUMN relay_id text REFERENCES smtp_relays(id);
ALTER TABLE messages ADD COLUMN fallback_relay_id text REFERENCES smtp_relays(id);
INSERT INTO schema_migrations(version) VALUES(8);
