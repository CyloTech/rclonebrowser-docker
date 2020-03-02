#!/bin/bash

set -u # Treat unset variables as an error.

trap "exit" TERM QUIT INT
trap "kill_rclonebrowser" EXIT

log() {
    echo "[rclonebrowsersupervisor] $*"
}

getpid_rclonebrowser() {
    PID=UNSET
    if [ -f /config/rclonebrowser.pid ]; then
        PID="$(cat /config/rclonebrowser.pid)"
        # Make sure the saved PID is still running and is associated to
        # RcloneBrowser.
        if [ ! -f /proc/$PID/cmdline ] || ! cat /proc/$PID/cmdline | grep -qw "rclone"; then
            PID=UNSET
        fi
    fi
    if [ "$PID" = "UNSET" ]; then
        PID="$(ps -o pid,args | grep -w "rclone" | grep -vw grep | tr -s ' ' | cut -d' ' -f2)"
    fi
    echo "${PID:-UNSET}"
}

is_rclonebrowser_running() {
    [ "$(getpid_rclonebrowser)" != "UNSET" ]
}

start_rclonebrowser() {
    xrdb /scripts/Xresources
    dbus-uuidgen
    export TERMINAL=xterm
    /usr/bin/rclone-browser > /logs/output.log 2>&1 &
}

kill_rclonebrowser() {
    PID="$(getpid_rclonebrowser)"
    if [ "$PID" != "UNSET" ]; then
        log "Terminating RcloneBrowser..."
        kill $PID
        wait $PID
    fi
}

set_config() {
    mkdir -p /config/xdg/config/rclone-browser/
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
http_proxy=
https_proxy=
iconSize=medium
lastUsedDestFolder=
lastUsedSourceFolder=
mount=--vfs-cache-mode writes --allow-other
no_proxy=
notifyFinishedTransfers=false
rclone=/usr/bin/rclone
rcloneConf=/rclone_config/rclone.conf
rowColors=true
showFileIcons=true
showFolderIcons=true
showHidden=true
stream=
useProxy=false
EOF
}

automount_rclone() {
    while read -r RCLONE_MOUNT
    do
        if [ -d /shared_mounts/"$RCLONE_MOUNT" ]; then
            rclone --config /rclone_config/rclone.conf mount "$RCLONE_MOUNT":/ /shared_mounts/"$RCLONE_MOUNT" --allow-other --uid "$USER_ID" --gid "$GROUP_ID" > /logs/automount_"$RCLONE_MOUNT"_output.log 2>&1 &
        else
            echo /shared_mounts/"$RCLONE_MOUNT" does not exist, not mounting!
        fi
    done < <(sed -nE 's/\[(.*)\]/\1/p' /rclone_config/rclone.conf)
}

if ! is_rclonebrowser_running; then
    log "RcloneBrowser not started yet.  Proceeding..."
    set_config
    automount_rclone
    start_rclonebrowser
fi

RCLONEBROWSER_NOT_RUNNING=0
while [ "$RCLONEBROWSER_NOT_RUNNING" -lt 5 ]
do
    if is_rclonebrowser_running; then
        RCLONEBROWSER_NOT_RUNNING=0
    else
        RCLONEBROWSER_NOT_RUNNING="$(expr $RCLONEBROWSER_NOT_RUNNING + 1)"
    fi
    sleep 1
done

log "RcloneBrowser no longer running.  Exiting..."
