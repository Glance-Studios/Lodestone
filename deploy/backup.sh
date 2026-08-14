#!/bin/sh
# Snapshot Lodestone's state directory.
#
# The ledgers are the part that cannot be reconstructed. Lose them and you lose
# the audit trail *and* the ability to reclaim anything: nothing else records
# which image belongs to which deploy, so every manifest in the registry becomes
# permanently unreferenced. The whole directory is a couple of megabytes, so
# there is no reason to be selective.
#
# Safe to run while the agent is live. The ledger writes to a temporary file and
# renames it into place, so a snapshot sees either the old file or the new one,
# never a half-written one.

set -eu

SRC=${LODESTONE_DATA_DIR:-/var/lib/lodestone}
DEST=${LODESTONE_BACKUP_DIR:-/var/backups/lodestone}
KEEP_DAYS=${LODESTONE_BACKUP_KEEP_DAYS:-30}

[ -d "$SRC" ] || { echo "no such directory: $SRC" >&2; exit 1; }

mkdir -p "$DEST"

stamp=$(date -u +%Y%m%dT%H%M%SZ)
out="$DEST/lodestone-$stamp.tar.gz"
tmp="$DEST/.lodestone-$stamp.tar.gz.part"

# Write beside the target then rename, so an interrupted run cannot leave a
# truncated archive that looks like a real backup.
tar -czf "$tmp" -C "$(dirname "$SRC")" "$(basename "$SRC")"
mv "$tmp" "$out"

find "$DEST" -maxdepth 1 -name 'lodestone-*.tar.gz' -mtime "+$KEEP_DAYS" -delete
find "$DEST" -maxdepth 1 -name '.lodestone-*.part' -mtime +1 -delete

echo "wrote $out ($(du -h "$out" | cut -f1))"
