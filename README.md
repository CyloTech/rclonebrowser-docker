# Rclone for Appbox

The Appbox Rclone app: [rclone](https://rclone.org/) with the Rclone Web UI,
mounts that are saved and restored, and an SSH login that runs rclone
commands only.

## What runs in the container

| Service | User | What it does |
|---|---|---|
| `rclone` | `rclone` | `rclone rcd` on 127.0.0.1:5572. Owns every mount. Accepts only the gateway's internal credentials. |
| `gateway` | `gateway` | Serves Rclone Web on port 5800 (`VIRTUAL_PORT`), checks logins, filters API calls, saves and restores mounts. |
| `sshd` | root | SSH on `$PORT`. Logins run `rclone-ssh`, which runs a limited set of rclone commands and never a shell. |
| `appbox-callback` | root | Sends the installed callback once after the first start. |

rclone is built from the `appbox/v1.75.2` branch of
[CyloTech/rclone](https://github.com/CyloTech/rclone): upstream v1.75.2 with
`lib/appbox`, which removes every option that runs an external program and
limits the local backend to `/APPBOX_DATA`, `/shared_mounts`, `/cache` and
`/rclone_config`. The Dockerfile pins the fork commit.

`appbox-rclone/` holds the gateway, the SSH login shell and the password tool
(one Go binary).

## Login

Username `rclone`, password from the `VNC_PASSWORD` install field. The first
start stores its hashes in `/rclone_config/.appbox`; after that the stored
password wins. `/moduser.sh <new_password>` changes it for the web UI and SSH
without restarting rclone, so mounts stay up. SSH needs at least 12
characters; shorter passwords work in the web UI only.

## Mounts

A mount made on the web UI's Mounts page must be a folder directly inside
`/shared_mounts`, so other apps on the Cylo can see it. The gateway saves each
successful mount in `/rclone_config/.appbox/automounts.json` with its options
and removes it when it is unmounted from the UI. On every start, and every 30
seconds, it remounts anything saved that isn't mounted. Defaults:
`CacheMode=full`, `AllowOther=true`, cache in `/cache`.

The first start of this version migrates the Rclone Browser auto-mounts: each
remote with a folder of the same name in `/shared_mounts` becomes a saved
mount, and the flags in `/rclone_config/mount_opts` are translated with
rclone's option table. Flags that aren't mount, VFS or main options (for
example backend flags such as `--drive-chunk-size`) are skipped and logged in
`/logs/gateway/current`.

## The web UI's API filter

The gateway allows only the API calls Rclone Web uses (see `rcAllowed` in
`appbox-rclone/gateway.go`). It also:

- answers `core/disks` with the app's folders instead of the host's mounts;
- allows `core/command` only for reading the config file;
- keeps the config file at `/rclone_config/rclone.conf`;
- refuses changes to rc, http and serve options and to the log file;
- refuses to create a browser-sign-in (OAuth) remote without a token and
  explains how to add it over SSH;
- refuses connections from public addresses, since the app's port may also
  be published directly.

## SSH

```bash
ssh -p <port> rclone@<domain> lsd gdrive:
ssh -t -L 53682:127.0.0.1:53682 -p <port> rclone@<domain> config
```

Commands: `config`, `authorize`, `listremotes`, `version`, `help`, `ls`,
`lsd`, `lsl`, `lsf`, `lsjson`, `tree`, `size`, `about`, `copy`, `copyto`,
`move`, `moveto`, `sync`, `check`. Password login only; only port 53682 can
be forwarded; no SFTP or SCP.

## Logs

`/logs/rclone`, `/logs/gateway` and `/logs/sshd` (s6-log, current file plus
rotated copies).

## Tests

```bash
tests/test-installed-callback.sh
docker build -t rclone-appbox:dev . && tests/test-image.sh rclone-appbox:dev
```

The image build also runs the Go tests in `appbox-rclone/`.
