#!/usr/bin/env bash
# Tests for the Appbox installed callback, run against a local mock API (needs
# bash, curl, awk and python3; no Docker):
#   - rootfs/usr/local/bin/appbox-installed-callback retries network errors,
#     408, 429 and 5xx, stops after one try on any other status and never logs
#     the callback token;
#   - the appbox-callback s6 service sends a pending callback once, clears the
#     flag and then idles;
#   - the first-boot block of cont-init.d/0-rclonebrowser.sh only marks the
#     callback pending and finishes without calling the API. The rest of that
#     script (FUSE, automount services, chown of system paths) needs the real
#     image, so only its tail from the first-boot check is run.
#
# Usage: tests/test-installed-callback.sh
set -u

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
helper="${repo_root}/rootfs/usr/local/bin/appbox-installed-callback"
token_id='999?callback_token=querytoken0123456789'
header_token='headertoken0123456789'

# Answers GET with 200 (readiness) and each POST with the next status from a
# comma-separated list, logging what it received.
mock_server='
import http.server, json, os, sys
port, codes, log = int(sys.argv[1]), [int(c) for c in sys.argv[2].split(",")], sys.argv[3]
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
    def do_POST(self):
        seen = sum(1 for _ in open(log)) if os.path.exists(log) else 0
        code = codes[min(seen, len(codes) - 1)]
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length).decode() if length else ""
        with open(log, "a") as f:
            f.write(json.dumps({"path": self.path, "code": code, "body": body,
                                "token": self.headers.get("X-Cylo-Callback-Token", "")}) + "\n")
        self.send_response(code)
        self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
'

failures=0
work=""
server=""

pass() { echo "ok   $*"; }
fail() {
    echo "FAIL $*"
    [[ -f "${work}/output" ]] && sed 's/^/     /' "${work}/output"
    failures=$((failures + 1))
}

start_mock() {
    work="$(mktemp -d)"
    port=$((20000 + RANDOM % 20000))
    python3 -c "${mock_server}" "${port}" "$1" "${work}/requests" &
    server=$!
    for _ in $(seq 1 50); do
        curl -s -o /dev/null "http://127.0.0.1:${port}/" && return 0
        sleep 0.1
    done
    echo "mock API did not start" >&2
    exit 2
}

stop_mock() {
    kill "${server}" 2>/dev/null
    wait "${server}" 2>/dev/null
    rm -rf "${work}"
}

# Run a command, killing it (exit 124) if it is still running after $1 seconds,
# so a callback that never stops fails the test instead of hanging it.
run_limited() {
    local limit=$1 pid ticks=0
    shift
    "$@" &
    pid=$!
    while kill -0 "${pid}" 2>/dev/null && [[ "${ticks}" -lt $((limit * 10)) ]]; do
        sleep 0.1
        ticks=$((ticks + 1))
    done
    if kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null
        wait "${pid}" 2>/dev/null
        return 124
    fi
    wait "${pid}"
}

request_count() { cat "${work}/requests" 2>/dev/null | wc -l | tr -d ' '; }
token_logged() { grep -cE 'querytoken|headertoken' "${work}/output"; }

# --- helper ------------------------------------------------------------------

helper_case() {
    local codes=$1 want_exit=$2 want_requests=$3
    shift 3
    start_mock "${codes}"
    run_limited 20 env INSTANCE_ID="${token_id}" CALLBACK_TOKEN="${header_token}" \
        APPBOX_CALLBACK_BASE="http://127.0.0.1:${port}" \
        APPBOX_CALLBACK_RETRY_DELAY=1 \
        "$@" "${helper}" >"${work}/output" 2>&1
    local rc=$? requests leaked
    requests=$(request_count)
    leaked=$(token_logged)
    if [[ "${rc}" == "${want_exit}" && "${requests}" == "${want_requests}" && "${leaked}" == 0 ]]; then
        pass "helper codes=${codes} exit=${rc} requests=${requests}"
    else
        fail "helper codes=${codes} exit=${rc}/${want_exit} requests=${requests}/${want_requests} token_logged=${leaked}"
    fi
    stop_mock
}

helper_case 200 0 1
helper_case 401 1 1
helper_case 404 1 1
helper_case 409 1 1
helper_case 422 1 1
helper_case 503,502,200 0 3
helper_case 429,408,204 0 3
helper_case 500,401 1 2
helper_case 200 0 0 SKIP_APPBOX_CALLBACK=1

# Network errors are retried: nothing listens on the port for the first try.
work="$(mktemp -d)"
port=$((20000 + RANDOM % 20000))
(
    sleep 1.5
    exec python3 -c "${mock_server}" "${port}" 200 "${work}/requests"
) &
server=$!
run_limited 20 env INSTANCE_ID="${token_id}" APPBOX_CALLBACK_BASE="http://127.0.0.1:${port}" \
    APPBOX_CALLBACK_RETRY_DELAY=1 "${helper}" >"${work}/output" 2>&1
rc=$?
if [[ "${rc}" == 0 ]] && grep -q 'HTTP 000, retrying' "${work}/output" && [[ "$(request_count)" == 1 ]]; then
    pass "helper retries a network error"
else
    fail "helper network error exit=${rc} requests=$(request_count)"
fi
stop_mock

# An invalid APPBOX_CALLBACK_RETRY_DELAY falls back to the 5s default.
start_mock 503,200
run_limited 20 env INSTANCE_ID="${token_id}" APPBOX_CALLBACK_BASE="http://127.0.0.1:${port}" \
    APPBOX_CALLBACK_RETRY_DELAY='1;x' "${helper}" >"${work}/output" 2>&1
rc=$?
if [[ "${rc}" == 0 ]] && grep -q 'retrying in 5s' "${work}/output" && [[ "$(request_count)" == 2 ]]; then
    pass "helper ignores an invalid retry delay"
else
    fail "helper invalid retry delay exit=${rc} requests=$(request_count)"
fi
stop_mock

# Token header, query string and optional body reach the API.
start_mock 200
run_limited 20 env INSTANCE_ID="${token_id}" CALLBACK_TOKEN="${header_token}" \
    APPBOX_CALLBACK_BASE="http://127.0.0.1:${port}" \
    "${helper}" '{"custom_fields":[{"key":"ADMIN_PANEL","value":"https://example.test/"}]}' \
    >"${work}/output" 2>&1
if python3 - "${work}/requests" "${header_token}" <<'PY'
import json, sys
req = json.loads(open(sys.argv[1]).readline())
assert req["path"] == "/v1/apps/installed/999?callback_token=querytoken0123456789", req
assert req["token"] == sys.argv[2], req
assert json.loads(req["body"])["custom_fields"][0]["key"] == "ADMIN_PANEL", req
PY
then
    pass "helper sends path, X-Cylo-Callback-Token and body"
else
    fail "helper request contents"
fi
stop_mock


# --- appbox-callback service ---------------------------------------------------

service_run="${repo_root}/rootfs/etc/services.d/appbox-callback/run"
cont_init="${repo_root}/rootfs/etc/cont-init.d/0-rclonebrowser.sh"
# Commands the cont-init script needs that only exist in the image are stubbed
# out, and image paths are moved into the sandbox (@WORK@ is the sandbox).
cont_init_stubs=()
cont_init_sed=()
cont_init_from='^if \[\[ ! -f /etc/app_configured \]\]; then$'

service_case() {
    local codes=$1 want_requests=$2 flag=$3
    start_mock "${codes}"
    local sandbox="${work}/run"
    sed -e '1s|.*|#!/usr/bin/env bash|' \
        -e "s|/etc/appbox_callback_pending|${work}/callback_pending|g" \
        -e "s|/usr/local/bin/appbox-installed-callback|${helper}|" \
        -e 's|^\( *\)exec s6-pause$|\1echo paused; exit 0|' \
        -e 's|^exec sleep infinity$|echo paused|' \
        "${service_run}" >"${sandbox}"
    chmod +x "${sandbox}"
    [[ "${flag}" == yes ]] && touch "${work}/callback_pending"
    run_limited 20 env INSTANCE_ID="${token_id}" CALLBACK_TOKEN="${header_token}" \
        APPBOX_CALLBACK_BASE="http://127.0.0.1:${port}" \
        APPBOX_CALLBACK_RETRY_DELAY=1 "${sandbox}" >"${work}/output" 2>&1
    local rc=$? requests
    requests=$(request_count)
    if [[ "${rc}" == 0 && "${requests}" == "${want_requests}" && ! -f "${work}/callback_pending" ]] \
        && grep -q '^paused$' "${work}/output" && [[ "$(token_logged)" == 0 ]]; then
        pass "service codes=${codes} flag=${flag}: requests=${requests}, flag cleared, service idles"
    else
        fail "service codes=${codes} flag=${flag}: exit=${rc} requests=${requests}/${want_requests}"
    fi
    stop_mock
}

service_case 401 1 yes
service_case 503,200 2 yes
service_case 200 0 no

# --- cont-init -----------------------------------------------------------------

# Runs the cont-init script with its markers moved into the sandbox and a curl
# on PATH that records every call and fails, so any synchronous callback (the
# old endless loop) shows up as a recorded call or a timeout.
cont_init_case() {
    local state=$1
    start_mock 401
    local sandbox="${work}/cont-init"
    mkdir -p "${work}/bin"
    printf '#!/bin/sh\necho "$@" >>"%s/curl_calls"\nexit 7\n' "${work}" >"${work}/bin/curl"
    local stub
    for stub in ${cont_init_stubs[@]+"${cont_init_stubs[@]}"}; do
        printf '#!/bin/sh\nexit 0\n' >"${work}/bin/${stub}"
    done
    chmod +x "${work}/bin/"*
    # cont_init_from (a regex) runs only the script's tail from the first
    # matching line, for scripts whose earlier setup needs the real image.
    if [[ -n "${cont_init_from:-}" ]]; then
        { echo; re="${cont_init_from}" awk 'f || $0 ~ ENVIRON["re"] { f = 1; print }' "${cont_init}"; } >"${work}/cont-init.src"
    else
        cp "${cont_init}" "${work}/cont-init.src"
    fi
    sed -e '1s|.*|#!/usr/bin/env bash|' \
        -e "s|/etc/app_configured|${work}/app_configured|g" \
        -e "s|/etc/appbox_callback_pending|${work}/callback_pending|g" \
        ${cont_init_sed[@]+"${cont_init_sed[@]/@WORK@/${work}}"} \
        "${work}/cont-init.src" >"${sandbox}"
    chmod +x "${sandbox}"
    [[ "${state}" == restart ]] && touch "${work}/app_configured"
    run_limited 5 env PATH="${work}/bin:${PATH}" INSTANCE_ID="${token_id}" \
        CALLBACK_TOKEN="${header_token}" "${sandbox}" >"${work}/output" 2>&1
    local rc=$? want_flag=yes have_flag=no
    [[ "${state}" == restart ]] && want_flag=no
    [[ -f "${work}/callback_pending" ]] && have_flag=yes
    if [[ "${rc}" == 0 && -f "${work}/app_configured" && "${have_flag}" == "${want_flag}" \
        && ! -f "${work}/curl_calls" && "$(request_count)" == 0 && "$(token_logged)" == 0 ]]; then
        pass "cont-init ${state}: finished without calling the API, pending flag=${have_flag}"
    else
        fail "cont-init ${state}: exit=${rc} pending=${have_flag}/${want_flag} curl_calls=$(cat "${work}/curl_calls" 2>/dev/null | wc -l | tr -d ' ')"
    fi
    stop_mock
}

cont_init_case fresh
cont_init_case restart

if grep -q 'apps/installed' "${cont_init}"; then
    fail "0-rclonebrowser.sh still calls the installed callback"
else
    pass "0-rclonebrowser.sh only marks the callback pending"
fi

# The pending flag is set before the configured marker, so a crash between the
# two cannot lose the callback.
pending_line=$(grep -n 'touch /etc/appbox_callback_pending' "${cont_init}" | head -n 1 | cut -d: -f1)
configured_line=$(grep -n 'touch /etc/app_configured' "${cont_init}" | head -n 1 | cut -d: -f1)
if [[ -n "${pending_line}" && -n "${configured_line}" && "${pending_line}" -lt "${configured_line}" ]]; then
    pass "0-rclonebrowser.sh marks the callback pending before /etc/app_configured"
else
    fail "0-rclonebrowser.sh must touch /etc/appbox_callback_pending before /etc/app_configured"
fi

exit "${failures}"
