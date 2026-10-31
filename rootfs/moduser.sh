#!/bin/bash
# Sets a new password for the web UI and SSH (username "rclone"). SSH
# needs at least 12 characters. Mounts stay up while it changes.
# Usage: /moduser.sh <new_password>
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
    echo "usage: /moduser.sh <new_password>" >&2
    exit 2
fi
printf '%s' "$1" | /usr/local/bin/appbox-rclone set-password
