#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
if [ -e .env ]; then
  echo '.env already exists; keeping existing credentials.'
  exit 0
fi
umask 077
mkdir -p secrets
cp .env.example .env
secret=$(openssl rand -base64 32)
password=$(openssl rand -hex 16)
seed_certificates=
seed_logo=
# Generated values contain no shell or sed metacharacters.
awk -v secret="$secret" -v password="$password" -v seed_certificates="$seed_certificates" -v seed_logo="$seed_logo" '
  /^SECRET_KEY=/ { print "SECRET_KEY=" secret; next }
  /^ROOT_PASSWORD=/ { print "ROOT_PASSWORD=" password; next }
  /^SEED_CERTIFICATES_FILE=/ { print "SEED_CERTIFICATES_FILE=" seed_certificates; next }
  /^SEED_LOGO_FILE=/ { print "SEED_LOGO_FILE=" seed_logo; next }
  { print }
' .env.example > .env
echo 'Created .env. Find your local root credentials there.'
echo 'Run: docker compose up --build -d'
