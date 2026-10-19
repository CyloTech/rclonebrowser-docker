#!/usr/bin/with-contenv bash

# groupmod -g "${ROOT_USER_ID}" nogroup
# usermod -g "${ROOT_USER_ID}" nobody
# usermod -u "${ROOT_USER_ID}" nobody

mkdir -p /var/tmp/nginx/

echo -e "user_allow_other" > /etc/fuse.conf

# Make sure mandatory directories exist.
mkdir -p /config
rm -rf /logs/*

# Take ownership of the config directory content.
rm -rf /config
chown -R "$USER_ID":"$GROUP_ID" /rclone_config
chown "$USER_ID":"$GROUP_ID" /cache
chown -R nobody:nobody /logs
chmod 777 /logs

if [ -f /config/xdg/config/rclone/rclone.conf ];then
    mv /config/xdg/config/rclone/rclone.conf /rclone_config/rclone.conf
fi

chown root:root /bin/fusermount
chmod +s /bin/fusermount

chown root:root /usr/bin/fusermount3
chmod +s /usr/bin/fusermount3

umount -f /shared_mounts/* || echo "Not already mounted"

set_config() {
    mkdir -p /config/xdg/config/rclone-browser/
    touch /rclone_config/mount_opts
    touch /rclone_config/http_proxy
    touch /rclone_config/https_proxy
    if [ -s /rclone_config/mount_opts ]; then
        MOUNT_OPTS=$(cat /rclone_config/mount_opts)
    else
        echo "--vfs-cache-mode full --allow-other" > /rclone_config/mount_opts
        MOUNT_OPTS="--vfs-cache-mode full --allow-other"
    fi

    if [ -s /rclone_config/http_proxy ]; then
        HTTP_PROXY=$(cat /rclone_config/http_proxy)
    fi

    if [ -s /rclone_config/https_proxy ]; then
        HTTPS_PROXY=$(cat /rclone_config/https_proxy)
    fi

    USE_PROXY="false"

    # If HTTP_PROXY or HTTPS_PROXY is set, set USE_PROXY to true
    if [ -n "$HTTP_PROXY" ] || [ -n "$HTTPS_PROXY" ]; then
        USE_PROXY="true"
    fi

    cat << EOF > /config/xdg/config/rclone-browser/rclone-browser.conf
[Settings]
alwaysShowInTray=false
checkRcloneBrowserUpdates=false
checkRcloneUpdates=false
closeToTray=false
darkMode=false
darkModeIni=false
defaultDownloadDir=
defaultDownloadOptions=
defaultRcloneOptions=--fast-list
defaultUploadDir=
defaultUploadOptions=
driveShared=0
http_proxy=${HTTP_PROXY}
https_proxy=${HTTPS_PROXY}
iconSize=medium
lastUsedDestFolder=
lastUsedSourceFolder=
mount=${MOUNT_OPTS} --cache-dir /cache
no_proxy=
notifyFinishedTransfers=false
rclone=/usr/bin/rclone
rcloneConf=/rclone_config/rclone.conf
rowColors=true
showFileIcons=true
showFolderIcons=true
showHidden=true
stream=
useProxy=${USE_PROXY}
EOF
}

automount_rclone() {
    # Delete any previous automounts
    rm -rf /etc/services.d/automount*
    rm -rf /etc/services.d/app/automount*
    rm -rf /var/run/s6/services/app/automount*
    rm -rf /var/run/s6/services/automount*

    MOUNT_OPTS=$(cat /rclone_config/mount_opts)

    while read -r RCLONE_MOUNT
    do
        # Remove new lines and return carriage
        RCLONE_MOUNT=$(echo $RCLONE_MOUNT | tr -d '\r' | tr -d '\n')        
        # Create a service for each one
        if [ -d "/shared_mounts/${RCLONE_MOUNT}" ]; then
            echo "Mounting /shared_mounts/${RCLONE_MOUNT}..."
            mkdir -p "/etc/services.d/automount_${RCLONE_MOUNT}/log"
            cat << EOF > "/etc/services.d/automount_${RCLONE_MOUNT}/log/run"
#!/bin/sh
exec logutil-service "/logs/automount_$RCLONE_MOUNT"
EOF
            echo "3" > "/etc/services.d/automount_${RCLONE_MOUNT}/notification-fd"
            cat << EOF > "/etc/services.d/automount_${RCLONE_MOUNT}/run"
#!/usr/bin/execlineb -P

# Redirect stderr to stdout.
fdmove -c 2 1

foreground { umount -f "/shared_mounts/$RCLONE_MOUNT" }

s6-setuidgid app

rclone --config /rclone_config/rclone.conf --drive-acknowledge-abuse mount "$RCLONE_MOUNT:/" "/shared_mounts/$RCLONE_MOUNT" --uid $USER_ID --gid $GROUP_ID $MOUNT_OPTS --cache-dir /cache
EOF
        chmod +x "/etc/services.d/automount_${RCLONE_MOUNT}/log/run"
        chmod +x "/etc/services.d/automount_${RCLONE_MOUNT}/run"
        cp -R "/etc/services.d/automount_${RCLONE_MOUNT}" /var/run/s6/services
        chown -R $ROOT_USER_ID:$ROOT_USER_ID /var/run/s6/services
        chown -R $ROOT_USER_ID:$ROOT_USER_ID "/etc/services.d/automount_${RCLONE_MOUNT}"
        else
            echo "/shared_mounts/${RCLONE_MOUNT} does not exist, not mounting!"
        fi
    done < <(sed -nE 's/\[(.*)\]/\1/p' /rclone_config/rclone.conf)
}

set_config
automount_rclone

xrdb /scripts/Xresources

if [[ ! -f /etc/app_configured ]]; then
    # The appbox-callback service sends the installed callback after
    # cont-init, so a slow or rejected callback cannot keep Rclone Browser
    # from starting. The flag is set first so a crash before the marker
    # cannot lose it.
    touch /etc/appbox_callback_pending
    touch /etc/app_configured
fi
