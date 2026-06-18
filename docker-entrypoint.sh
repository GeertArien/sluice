#!/bin/sh
# Drop-privileges entrypoint for unraid/NAS compatibility.
#
# Bind-mounted volumes keep their host ownership, so a container that runs as
# a fixed UID can't write to a data dir owned by another user (on unraid the
# appdata share is nobody:users = 99:100). Starting as root, we remap the
# bundled `sluice` user to the requested PUID/PGID, take ownership of the data
# dir and home, then exec the app as that unprivileged user via su-exec.
#
# If the container was already started as a non-root user (e.g. `docker run
# --user 99:100`), we skip the remap and just run — the caller is responsible
# for the data dir being writable in that case.
set -e

DATA_DIR="${SLUICE_DATA_DIR:-/data}"

if [ "$(id -u)" = "0" ]; then
	PUID="${PUID:-1000}"
	PGID="${PGID:-1000}"

	# Align the sluice group/user to the requested ids (-o allows reuse of an
	# id that already exists, e.g. PGID=100 "users" on unraid).
	if [ "$(id -g sluice)" != "$PGID" ]; then
		groupmod -o -g "$PGID" sluice
	fi
	if [ "$(id -u sluice)" != "$PUID" ]; then
		usermod -o -u "$PUID" sluice
	fi

	# Home must be writable for git config and ~/.ssh; remap may have orphaned it.
	chown sluice:sluice /home/sluice 2>/dev/null || true

	# Only chown the data dir when it isn't already ours — a recursive chown of
	# large mirror workspaces on every start would be slow.
	if [ "$(stat -c %u "$DATA_DIR")" != "$PUID" ] || [ "$(stat -c %g "$DATA_DIR")" != "$PGID" ]; then
		echo "sluice: taking ownership of $DATA_DIR for ${PUID}:${PGID}"
		chown -R sluice:sluice "$DATA_DIR"
	fi

	export HOME=/home/sluice
	exec su-exec sluice "$@"
fi

# Non-root start: run as-is.
export HOME="${HOME:-/home/sluice}"
exec "$@"
