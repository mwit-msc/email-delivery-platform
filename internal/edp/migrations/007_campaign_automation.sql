ALTER TABLE api_keys ADD COLUMN created_by text REFERENCES users(id);
ALTER TABLE campaigns ADD COLUMN mapping jsonb NOT NULL DEFAULT '{}';
CREATE TABLE email_assets(id text PRIMARY KEY, application_id text NOT NULL REFERENCES applications(id), name text NOT NULL, mime_type text NOT NULL, data bytea NOT NULL, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE webhook_endpoints(id text PRIMARY KEY, application_id text NOT NULL REFERENCES applications(id), name text NOT NULL, url text NOT NULL, secret text NOT NULL, enabled boolean NOT NULL DEFAULT true, created_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE webhook_jobs(id bigserial PRIMARY KEY, endpoint_id text NOT NULL REFERENCES webhook_endpoints(id), payload jsonb NOT NULL, event_id bigint NOT NULL, attempts integer NOT NULL DEFAULT 0, available_at timestamptz NOT NULL DEFAULT now(), lease_until timestamptz, lease_token text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'queued', last_error text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(endpoint_id,event_id));
CREATE INDEX webhook_queue ON webhook_jobs(available_at) WHERE status IN ('queued','sending');
CREATE FUNCTION enqueue_delivery_webhook() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO webhook_jobs(endpoint_id,event_id,payload)
  SELECT e.id,NEW.id,jsonb_build_object('event_id',NEW.id,'message_id',m.id,'application_id',m.application_id,'campaign_id',m.campaign_id,'status',NEW.status,'detail',NEW.detail,'created_at',NEW.created_at)
  FROM messages m JOIN webhook_endpoints e ON e.application_id=m.application_id AND e.enabled WHERE m.id=NEW.message_id;
  RETURN NEW;
END;
$$;
CREATE TRIGGER delivery_webhook AFTER INSERT ON delivery_events FOR EACH ROW EXECUTE FUNCTION enqueue_delivery_webhook();
INSERT INTO schema_migrations(version) VALUES(7);
