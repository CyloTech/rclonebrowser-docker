#!/usr/bin/env bash
# Release gate: run with the exact built image reference as the only argument.
# Exits zero only when the callback and image tests pass for that image and
# the authenticated registry reports that exact tag as definitively not found.
# Every other registry response (found, auth, transport, server error) fails.
set -Eeuo pipefail

cd "$(dirname "$0")/.."

if [[ $# -ne 1 ]]; then
    echo "usage: tests/release-gate.sh <image-reference>" >&2
    exit 2
fi

image_ref="$1"
case "${image_ref}" in
    repo.cylo.net/[a-z0-9._/-]*:[A-Za-z0-9._-]*) ;;
    *) echo "unexpected image reference: ${image_ref}" >&2; exit 2 ;;
esac

registry="${image_ref%%/*}"
repo_and_tag="${image_ref#*/}"
repo="${repo_and_tag%:*}"
tag="${repo_and_tag##*:}"

bash tests/test-installed-callback.sh
bash tests/test-image.sh "${image_ref}"

# Appbox's Docker gives files root owns in the image to the Cylo's own user
# ID and runs the app as USER_ID = that ID + 1000. Run the tests again on a
# copy of the image with that ownership (set-user-ID bits kept), as the
# app sees it in production.
# Image names must be lowercase; builder job ids are mixed case.
owned_ref="rclone-gate-owned-$(printf '%s' "${CYLO_JOB:-$$}" | tr '[:upper:]' '[:lower:]'):test"
case "${owned_ref}" in *[!A-Za-z0-9._:-]*) echo "bad CYLO_JOB" >&2; exit 2 ;; esac
trap 'docker image rm "${owned_ref}" >/dev/null 2>&1 || true' EXIT
printf '%s\n' \
    "FROM ${image_ref}" \
    "RUN find / -xdev -type f -perm -4000 > /tmp/suid \\" \
    "    && find / -xdev \\( -path /proc -o -path /sys -o -path /dev -o -path /etc/hosts -o -path /etc/resolv.conf -o -path /etc/hostname \\) -prune -o -user 0 -exec chown -h 54888096:54888096 {} + \\" \
    "    && xargs -r chmod u+s < /tmp/suid && rm /tmp/suid" \
    | docker build --builder default --platform linux/amd64 -q -t "${owned_ref}" - >/dev/null
CYLO_JOB="${CYLO_JOB:-$$}-owned" TEST_USER_ID=54889096 bash tests/test-image.sh "${owned_ref}"

# The registry credential goes to curl on stdin, not on its command line.
auth="$(python3 -I -c 'import json,sys,os; print(json.load(open(os.path.expanduser("~/.docker/config.json")))["auths"][sys.argv[1]]["auth"])' "${registry}")"
status="$(printf 'header = "Authorization: Basic %s"\n' "${auth}" | curl -s -o /dev/null -w '%{http_code}' --max-time 30 \
    --config - \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://${registry}/v2/${repo}/manifests/${tag}")" || status="curl-failed"
unset auth

case "${status}" in
    404) echo "registry reports ${image_ref} not found; release may proceed" ;;
    200) echo "tag already exists in the registry: ${image_ref}" >&2; exit 1 ;;
    *)   echo "indeterminate registry response (${status}) for ${image_ref}" >&2; exit 1 ;;
esac
