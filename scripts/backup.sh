#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
umask 077
mkdir -p backups
file=${1:-backups/edp-$(date -u +%Y%m%dT%H%M%SZ).dump}
docker compose exec -T postgres pg_dump -U edp -d edp --format=custom > "$file"
echo "Backup saved: $file"
