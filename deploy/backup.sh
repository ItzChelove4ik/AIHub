#!/bin/sh
# Daily AIHub database backup (run by aihub-backup.timer). A consistent
# copy (VACUUM INTO) lands in /var/backups/aihub; copies older than
# KEEP_DAYS are deleted. The provider keys inside are encrypted with
# AIHUB_MASTER_KEY from /opt/aihub/.env: keep that key somewhere safe and
# separate, or the backups cannot be used.
set -eu
KEEP_DAYS=${KEEP_DAYS:-14}
cd /opt/aihub
name="aihub-$(date -u +%Y-%m-%dT%H%M%SZ).db"
docker compose exec -T aihub-backend aihub-backend backup "/backups/$name"
chmod 0600 "/var/backups/aihub/$name"
find /var/backups/aihub -name 'aihub-*.db' -type f -mtime +"$KEEP_DAYS" -delete
