ALTER TABLE messages ADD COLUMN otp_encrypted boolean NOT NULL DEFAULT false;
ALTER TABLE messages ADD CONSTRAINT encrypted_otp_class CHECK(NOT otp_encrypted OR class='otp');
CREATE TABLE otp_challenges (
  id text PRIMARY KEY,
  application_id text NOT NULL REFERENCES applications(id),
  recipient text NOT NULL,
  purpose text NOT NULL,
  code_hash bytea NOT NULL,
  attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
  consumed_at timestamptz,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at timestamptz NOT NULL,
  message_id text NOT NULL UNIQUE REFERENCES messages(id),
  idempotency_key text NOT NULL,
  payload_hash text NOT NULL,
  UNIQUE(application_id,idempotency_key)
);
CREATE INDEX otp_recipient_created ON otp_challenges(application_id,lower(recipient),created_at DESC);
CREATE INDEX otp_expiration ON otp_challenges(expires_at) WHERE octet_length(code_hash)>0;
INSERT INTO schema_migrations(version) VALUES(15);
