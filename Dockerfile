# syntax=docker/dockerfile:1
#
# Rclone for Appbox: rclone with the Rclone Web UI, mounts that are saved
# and restored, and an SSH login that runs rclone commands only.

# rclone comes from the CyloTech fork: upstream rclone with the Appbox
# restrictions in lib/appbox (no external programs, local paths limited
# to the app's folders).
ARG RCLONE_VERSION=v1.75.2
ARG RCLONE_COMMIT=71fcb8ebd9a77c46a5fe4701ead9dff9bf4dd85d

FROM golang:1.26-alpine AS rclone
ARG RCLONE_VERSION
ARG RCLONE_COMMIT
RUN apk add --no-cache git
WORKDIR /src
RUN git init -q . \
    && git remote add origin https://github.com/CyloTech/rclone.git \
    && git fetch -q --depth 1 origin "${RCLONE_COMMIT}" \
    && git checkout -q FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "${RCLONE_COMMIT}"
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -X github.com/rclone/rclone/fs.Version=${RCLONE_VERSION}" \
        -o /out/rclone . \
    && mkdir -p /out/rclone-web \
    && unzip -q cmd/gui/dist.zip -d /out/rclone-web

# The gateway, the SSH login shell and the password tool.
FROM golang:1.26-alpine AS helper
WORKDIR /src
COPY appbox-rclone/go.mod appbox-rclone/go.sum ./
RUN go mod download
COPY appbox-rclone/ ./
RUN CGO_ENABLED=0 go test ./... \
    && CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/appbox-rclone .

FROM alpine:3.24
RUN apk add --no-cache \
        bash \
        ca-certificates \
        curl \
        fuse3 \
        openssh-server \
        openssl \
        s6-overlay \
        tzdata \
    && echo user_allow_other > /etc/fuse.conf \
    && chmod 4755 /usr/bin/fusermount3 \
    && addgroup -S -g 990 gateway \
    && adduser -S -D -H -u 990 -G gateway -h /nonexistent -s /sbin/nologin gateway \
    && echo /usr/local/bin/rclone-ssh >> /etc/shells \
    && rm -f /etc/motd
COPY --from=rclone /out/rclone /usr/bin/rclone
COPY --from=rclone /out/rclone-web /usr/share/rclone-web
COPY --from=helper /out/appbox-rclone /usr/local/bin/appbox-rclone
COPY rootfs/ /
RUN ln -s appbox-rclone /usr/local/bin/rclone-ssh \
    && chmod 755 /moduser.sh /usr/local/bin/appbox-installed-callback \
        /etc/cont-init.d/* /etc/services.d/*/run /etc/services.d/*/log/run \
    && rclone version

ENV S6_BEHAVIOUR_IF_STAGE2_FAILS=2 \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0 \
    S6_KILL_GRACETIME=8000 \
    S6_SERVICES_GRACETIME=8000

EXPOSE 5800
ENTRYPOINT ["/init"]
