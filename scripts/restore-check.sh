#!/bin/sh
# Restore into a fresh database without replacing the running application's data.
set -eu
cd "$(dirname "$0")/.."
file=${1:?Usage: scripts/restore-check.sh BACKUP.dump}
restore_db="edp_restore_$(date -u +%Y%m%d%H%M%S)"
docker compose exec -T postgres createdb -U edp "$restore_db"
docker compose exec -T postgres pg_restore -U edp --no-owner --dbname "$restore_db" < "$file"
docker compose exec -T postgres psql -U edp -d "$restore_db" -c 'SELECT count(*) AS messages FROM messages; SELECT count(*) AS templates FROM templates;'
echo "Restore verified in $restore_db. Existing application data is unchanged."
