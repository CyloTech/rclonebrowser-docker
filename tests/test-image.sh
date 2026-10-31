#!/usr/bin/env bash
# End-to-end tests for the Rclone image. Starts the image with FUSE and a
# client container on a private Docker network, then checks the web UI
# login and API filter, mounts being saved and restored across restarts,
# the SSH login, password resets and the migration of old auto-mounts.
#
# Usage: tests/test-image.sh <image>
# Needs Docker with /dev/fuse. Everything it creates is removed on exit.
set -u

image=${1:?usage: tests/test-image.sh <image>}
run_id="rclone-test-$$"
net="${run_id}-net"
app="${run_id}-app"
client="${run_id}-client"
pass1='first-password-123'
pass2='second-password-456'
failures=0

pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; failures=$((failures + 1)); }
check() { local name=$1; shift; if "$@" >/dev/null 2>&1; then pass "${name}"; else fail "${name}"; fi; }

cleanup() {
    docker rm -f "${app}" "${app}-legacy" "${client}" >/dev/null 2>&1
    docker volume rm "${run_id}-config" "${run_id}-data" "${run_id}-shared" "${run_id}-legacy-config" "${run_id}-legacy-data" "${run_id}-legacy-shared" >/dev/null 2>&1
    docker network rm "${net}" >/dev/null 2>&1
}
trap cleanup EXIT

docker network create "${net}" >/dev/null
docker run -d --name "${client}" --network "${net}" alpine:3.24 sleep 3600 >/dev/null
docker exec "${client}" apk add -q --no-cache curl jq openssh-client sshpass >/dev/null

start_app() {
    local name=$1 prefix=$2 password=$3
    docker run -d --name "${name}" --network "${net}" --network-alias "${name}" \
        --cap-add SYS_ADMIN --device /dev/fuse --security-opt apparmor=unconfined \
        -e USER_ID=1000 -e GROUP_ID=1000 -e VNC_PASSWORD="${password}" \
        -e PORT=2222 -e DOMAIN="${name}" -e SKIP_APPBOX_CALLBACK=1 \
        -v "${prefix}-config:/rclone_config" -v "${prefix}-data:/APPBOX_DATA" \
        -v "${prefix}-shared:/shared_mounts" \
        "${image}" >/dev/null
}

wait_ready() {
    local name=$1
    for _ in $(seq 1 60); do
        if docker exec "${client}" curl -fsS "http://${name}:5800/healthz" >/dev/null 2>&1 &&
            docker exec "${client}" sh -c "nc -z ${name} 2222" >/dev/null 2>&1; then
            sleep 2
            return 0
        fi
        sleep 1
    done
    echo "${name} didn't become ready" >&2
    docker logs "${name}" 2>&1 | tail -40 >&2
    return 1
}

# rc <host> <password> <path> [json] prints "<status> <body>".
rc() {
    local host=$1 pw=$2 path=$3 body=${4:-'{}'}
    docker exec "${client}" sh -c 'curl -s -o /tmp/out -w "%{http_code}" -u "rclone:$1" -H "Content-Type: application/json" -d "$2" "http://$3:5800/rc/$4"; echo " $(cat /tmp/out)"' _ "${pw}" "${body}" "${host}" "${path}"
}
status_of() { head -n 1 <<<"$1" | cut -d' ' -f1; }

# sshc <password> <command...> runs a command over SSH and prints its output.
sshc() {
    local pw=$1; shift
    docker exec "${client}" sshpass -p "${pw}" ssh -p 2222 -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o PubkeyAuthentication=no \
        "rclone@${app}" "$@" 2>&1
}

mounted_in_app() { docker exec "$1" sh -c "grep -q ' $2 fuse' /proc/mounts"; }

docker volume create "${run_id}-config" >/dev/null
docker volume create "${run_id}-data" >/dev/null
docker volume create "${run_id}-shared" >/dev/null
start_app "${app}" "${run_id}" "${pass1}"
wait_ready "${app}" || exit 1

# --- start-up -------------------------------------------------------------------
check "rclone runs as the rclone user" docker exec "${app}" sh -c "ps -o user,args | grep -q '^rclone .*rclone rcd'"
check "gateway runs as the gateway user" docker exec "${app}" sh -c "ps -o user,args | grep -q '^gateway .*appbox-rclone gateway'"
check "rclone's environment has no password" docker exec "${app}" sh -c '! tr "\0" "\n" < /proc/$(pgrep -f "rclone rcd")/environ | grep -q VNC_PASSWORD'
check "rclone API listens on localhost only" docker exec "${app}" sh -c "! netstat -ltn | grep 5572 | grep -v 127.0.0.1"
check "state is owned by the gateway" docker exec "${app}" sh -c '[ "$(stat -c %U /rclone_config/.appbox)" = gateway ]'

# --- web UI ---------------------------------------------------------------------
check "GUI page is served" docker exec "${client}" sh -c "curl -fsS http://${app}:5800/ | grep -qi '<html'"
loc=$(docker exec "${client}" curl -s -o /dev/null -w '%{redirect_url}' -H 'X-Forwarded-Proto: https' "http://${app}:5800/login")
[[ "${loc}" == *"url=https%3A%2F%2F${app}%3A5800%2Frc"* ]] && pass "login page gets the API address" || fail "login redirect: ${loc}"
[[ $(status_of "$(rc "${app}" wrong core/version)") == 401 ]] && pass "wrong password refused" || fail "wrong password"
r=$(rc "${app}" "${pass1}" core/version)
[[ $(status_of "$r") == 200 && "$r" == *'"version": "v1.75.2"'* ]] && pass "login works, rclone v1.75.2" || fail "core/version: $r"
[[ $(status_of "$(rc "${app}" "${pass1}" core/quit)") == 403 ]] && pass "core/quit blocked" || fail "core/quit"
[[ $(status_of "$(rc "${app}" "${pass1}" core/command '{"command":"ls","arg":["/etc"]}')") == 403 ]] && pass "core/command blocked" || fail "core/command"
r=$(rc "${app}" "${pass1}" core/disks)
[[ "$r" == *'/APPBOX_DATA'* && "$r" != *'"/"'* ]] && pass "core/disks lists the app's folders only" || fail "core/disks: $r"

docker exec "${app}" sh -c 'mkdir -p /APPBOX_DATA/data && echo hello > /APPBOX_DATA/data/hello.txt && chown -R rclone /APPBOX_DATA/data'
r=$(rc "${app}" "${pass1}" config/create '{"name":"local1","type":"alias","parameters":{"remote":"/APPBOX_DATA/data"}}')
[[ $(status_of "$r") == 200 ]] && pass "remote created" || fail "config/create alias: $r"
r=$(rc "${app}" "${pass1}" operations/list '{"fs":"/etc","remote":""}')
[[ $(status_of "$r") != 200 && "$r" == *outside* ]] && pass "local paths outside the app's folders refused" || fail "operations/list /etc: $r"
rc "${app}" "${pass1}" config/create '{"name":"evil","type":"sftp","parameters":{"host":"127.0.0.1","ssh":"sh -c id"}}' >/dev/null
r=$(rc "${app}" "${pass1}" operations/list '{"fs":"evil:","remote":""}')
[[ "$r" == *disabled* ]] && pass "SFTP ssh option can't run a program" || fail "sftp ssh: $r"
r=$(rc "${app}" "${pass1}" config/create '{"name":"gd","type":"drive","parameters":{}}')
[[ $(status_of "$r") == 400 && "$r" == *'ssh -t -L 53682'* ]] && pass "OAuth remote points to SSH" || fail "drive create: $r"

# --- mounts ---------------------------------------------------------------------
r=$(rc "${app}" "${pass1}" mount/mount '{"fs":"local1:","mountPoint":"/tmp/x"}')
[[ $(status_of "$r") == 400 ]] && pass "mount outside /shared_mounts refused" || fail "mount /tmp/x: $r"
r=$(rc "${app}" "${pass1}" mount/mount '{"fs":"local1:","mountPoint":"/shared_mounts/local1"}')
[[ $(status_of "$r") == 200 ]] && pass "mount created" || fail "mount/mount: $r"
check "mounted file is readable" docker exec "${app}" sh -c 'grep -q hello /shared_mounts/local1/hello.txt'
check "mount allows other users" docker exec "${app}" sh -c "grep ' /shared_mounts/local1 ' /proc/mounts | grep -q allow_other"
r=$(rc "${app}" "${pass1}" mount/listmounts)
[[ "$r" == *'/shared_mounts/local1'* ]] && pass "mount listed in the web UI" || fail "listmounts: $r"

docker restart "${app}" >/dev/null
wait_ready "${app}" || exit 1
for _ in $(seq 1 20); do mounted_in_app "${app}" /shared_mounts/local1 && break; sleep 1; done
r=$(rc "${app}" "${pass1}" mount/listmounts)
[[ "$r" == *'/shared_mounts/local1'* ]] && pass "mount restored after restart and listed" || fail "after restart: $r"
check "restored mount works" docker exec "${app}" sh -c 'grep -q hello /shared_mounts/local1/hello.txt'

docker exec "${app}" sh -c 'kill -9 $(pgrep -f "rclone rcd")'
for _ in $(seq 1 45); do r=$(rc "${app}" "${pass1}" mount/listmounts); [[ "$r" == *'/shared_mounts/local1'* ]] && break; sleep 1; done
[[ "$r" == *'/shared_mounts/local1'* ]] && pass "mount restored after rclone crashed" || fail "after crash: $r"
check "mount works after the crash" docker exec "${app}" sh -c 'grep -q hello /shared_mounts/local1/hello.txt'

# --- SSH ------------------------------------------------------------------------
out=$(sshc "${pass1}" version)
[[ "$out" == *"rclone v1.75.2"* ]] && pass "ssh runs rclone version" || fail "ssh version: $out"
out=$(sshc "${pass1}" lsf local1:)
[[ "$out" == *hello.txt* ]] && pass "ssh lists a remote" || fail "ssh lsf: $out"
out=$(sshc "${pass1}")
[[ "$out" == *"There is no shell"* ]] && pass "ssh without a command shows usage" || fail "ssh usage: $out"
for cmd in "bash" "sh -c id" "mount local1: /shared_mounts/x" "lsd local1: --config /tmp/x" "ls /etc" "serve http local1:"; do
    out=$(sshc "${pass1}" "${cmd}")
    if [[ "$out" == *uid=* || "$out" == *passwd* ]]; then fail "ssh '${cmd}' ran"; else pass "ssh '${cmd}' refused"; fi
done
out=$(docker exec "${client}" sh -c "echo ls | sshpass -p '${pass1}' sftp -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PubkeyAuthentication=no rclone@${app} 2>&1")
[[ "$out" != *hello* && "$out" == *"Connection closed"* ]] && pass "sftp not available" || fail "sftp: $out"
out=$(docker exec "${client}" sh -c "sshpass -p '${pass1}' ssh -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o PubkeyAuthentication=no -f -N -L 15572:127.0.0.1:5572 rclone@${app} && sleep 1 && curl -s -m 5 http://127.0.0.1:15572/ ; echo rc=\$?")
if [[ "$out" != *'"error"'* && "$out" != *Unauthorized* ]] && docker exec "${app}" grep -q 'port 5572, but the request was denied' /logs/sshd/current; then
    pass "port forwarding to the API refused"
else
    fail "forward to 5572: $out"
fi

# --- password reset -------------------------------------------------------------
docker exec "${app}" /moduser.sh "${pass2}" >/dev/null
[[ $(status_of "$(rc "${app}" "${pass1}" core/version)") == 401 ]] && pass "old web password stops working" || fail "old password still works"
[[ $(status_of "$(rc "${app}" "${pass2}" core/version)") == 200 ]] && pass "new web password works" || fail "new web password"
out=$(sshc "${pass2}" version)
[[ "$out" == *"rclone v1.75.2"* ]] && pass "new SSH password works" || fail "ssh new password: $out"
check "mounts stay up during a reset" docker exec "${app}" sh -c 'grep -q hello /shared_mounts/local1/hello.txt'
docker exec "${app}" /moduser.sh 'short123' >/dev/null
out=$(sshc 'short123' version)
[[ "$out" != *"rclone v1.75.2"* ]] && pass "short password keeps SSH off" || fail "short password allowed SSH"
[[ $(status_of "$(rc "${app}" short123 core/version)") == 200 ]] && pass "short password works in the web UI" || fail "short web password"
docker exec "${app}" /moduser.sh "${pass2}" >/dev/null
docker restart "${app}" >/dev/null
wait_ready "${app}" || exit 1
[[ $(status_of "$(rc "${app}" "${pass2}" core/version)") == 200 ]] && pass "reset password survives a restart" || fail "password reverted after restart"

# --- unmount --------------------------------------------------------------------
r=$(rc "${app}" "${pass2}" mount/unmount '{"mountPoint":"/shared_mounts/local1"}')
[[ $(status_of "$r") == 200 ]] && pass "unmounted from the web UI" || fail "unmount: $r"
docker restart "${app}" >/dev/null
wait_ready "${app}" || exit 1
sleep 3
r=$(rc "${app}" "${pass2}" mount/listmounts)
[[ "$r" != *'/shared_mounts/local1'* ]] && pass "unmounted mount stays off after restart" || fail "came back: $r"

# --- migration from the Rclone Browser versions ------------------------------------
for v in config data shared; do docker volume create "${run_id}-legacy-${v}" >/dev/null; done
docker run --rm -v "${run_id}-legacy-config:/c" -v "${run_id}-legacy-data:/d" -v "${run_id}-legacy-shared:/s" alpine:3.24 sh -c '
    printf "[legacy]\ntype = alias\nremote = /APPBOX_DATA/old\n\n[unused]\ntype = alias\nremote = /APPBOX_DATA/old\n" > /c/rclone.conf
    echo "--vfs-cache-mode full --allow-other --buffer-size 32M --drive-chunk-size 64M" > /c/mount_opts
    mkdir -p /d/old /s/legacy && echo migrated > /d/old/file.txt
    chown -R 1000:1000 /c /d /s'
start_app "${app}-legacy" "${run_id}-legacy" "${pass1}"
wait_ready "${app}-legacy" || exit 1
for _ in $(seq 1 20); do mounted_in_app "${app}-legacy" /shared_mounts/legacy && break; sleep 1; done
check "old folder-based auto-mount migrated" docker exec "${app}-legacy" sh -c 'grep -q migrated /shared_mounts/legacy/file.txt'
check "remote without a folder not mounted" docker exec "${app}-legacy" sh -c "! grep -q ' /shared_mounts/unused ' /proc/mounts"
check "skipped mount_opts flags are logged" docker exec "${app}-legacy" sh -c "grep -q 'drive-chunk-size' /logs/gateway/current"
r=$(rc "${app}-legacy" "${pass1}" options/get)
[[ "$r" == *'"BufferSize": 33554432'* ]] && pass "mount_opts main options applied" || fail "BufferSize not applied"

echo
if [[ "${failures}" == 0 ]]; then echo "all image tests passed"; else echo "${failures} image test(s) failed"; fi
exit "${failures}"
