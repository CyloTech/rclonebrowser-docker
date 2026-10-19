#
# RcloneBrowser Dockerfile
#

FROM jlesage/baseimage-gui:alpine-3.9-glibc

# Define environment variables
ENV RCLONE_INSTALL_VERSION=current
ENV ARCH=amd64

# Define working directory.
WORKDIR /tmp

# Install Rclone Browser dependencies

RUN echo "make sure you run cp ~/Development/Current/rclone/bin/rclone ~/Development/Current/rclonebrowser-app/scripts/"

RUN add-pkg \
      ca-certificates \
      fuse \
      fuse3 \
      curl \
      wget \
      qt5-qtbase \
      qt5-qtbase-x11 \
      libstdc++ \
      libgcc \
      dbus \
      xterm \
      bash \
      bc \
      openssh-client \
      sed \
    && add-pkg --virtual=build-dependencies \
        build-base \
        cmake \
        make \
        gcc \
        git \
        qt5-qtbase qt5-qtmultimedia-dev qt5-qttools-dev && \
        echo "user_allow_other" >> /etc/fuse.conf && \

# Compile RcloneBrowser
    git clone https://github.com/kapitainsky/RcloneBrowser.git /tmp && \
    mkdir /tmp/build && \
    cd /tmp/build && \
    cmake .. && \
    cmake --build . && \
    ls -l /tmp/build && \
    cp /tmp/build/build/rclone-browser /usr/bin  && \

    # cleanup
     del-pkg build-dependencies && \
    rm -rf /tmp/*

# Maximize only the main/initial window.
RUN \
    sed-patch 's/<application type="normal">/<application type="normal" title="Rclone Browser">/' \
        /etc/xdg/openbox/rc.xml && \
    sed -i '/sync/d' /etc/services.d/app/finish && \
    sed -i '/sync/d' /etc/cont-init.d/00-app-script.sh && \
    sed -i '/vim/a umount -f /shared_mounts/*' /etc/services.d/app/finish


# Download and install app icon directly (skip install_app_icon.sh which requires Node.js >= 20).
RUN \
    APP_ICON_URL=https://github.com/rclone/rclone/raw/master/graphics/logo/logo_symbol/logo_symbol_color_512px.png && \
    mkdir -p /opt/noVNC/app/images/icons && \
    wget -q -O /opt/noVNC/app/images/icons/master_icon.png "$APP_ICON_URL" && \
    for size in 16 32 48 64 128 256; do \
        cp /opt/noVNC/app/images/icons/master_icon.png /opt/noVNC/app/images/icons/icon_${size}x${size}.png; \
    done

# Add files.
COPY rootfs/ /
COPY VERSION /

# Set environment variables.
ENV APP_NAME="RcloneBrowser" \
    S6_KILL_GRACETIME=8000

RUN mkdir /scripts; sed -i '/sync/d' /etc/cont-init.d/00-app-script.sh
COPY scripts/ /scripts/

# Install our hard link from our no_local repo
COPY ./scripts/rclone /usr/bin
