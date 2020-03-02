#!/usr/bin/with-contenv bash

set -e # Exit immediately if a command exits with a non-zero status.
set -u # Treat unset variables as an error.

echo -e "user_allow_other" > /etc/fuse.conf

log() {
    echo "[cont-init.d] $(basename $0): $*"
}

# Make sure mandatory directories exist.
mkdir -p /config
rm -f /logs/*

# Take ownership of the config directory content.
chown -R "$USER_ID":"$GROUP_ID" /config
chown -R "$USER_ID":"$GROUP_ID" /rclone_config
chown -R "$USER_ID":"$GROUP_ID" /logs

if [ -f /config/xdg/config/rclone/rclone.conf ];then
    mv /config/xdg/config/rclone/rclone.conf /rclone_config/rclone.conf
fi

chown root:root /bin/fusermount
chmod +s /bin/fusermount
fusermount -u /shared_mounts/* || echo "None mounted"

while read -r RCLONE_MOUNT
do
    umount -f /shared_mounts/"$RCLONE_MOUNT" || echo "Not already mounted"
done < <(sed -nE 's/\[(.*)\]/\1/p' /rclone_config/rclone.conf)

if [[ ! -f /etc/app_configured ]]; then
    touch /etc/app_configured
    until [[ $(curl -i -H "Accept: application/json" -H "Content-Type:application/json" -X POST "https://api.cylo.io/v1/apps/installed/${INSTANCE_ID}" | grep '200') ]]
        do
        sleep 5
    done
fi

# Take ownership of the output directory.
#if ! chown $USER_ID:$GROUP_ID /output; then
    # Failed to take ownership of /output.  This could happen when,
    # for example, the folder is mapped to a network share.
    # Continue if we have write permission, else fail.
#    if s6-setuidgid $USER_ID:$GROUP_ID [ ! -w /output ]; then
#        log "ERROR: Failed to take ownership and no write permission on /output."
#        exit 1
#    fi
#fi

# vim: set ft=sh :
