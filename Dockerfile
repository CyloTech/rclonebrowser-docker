#
# RcloneBrowser Dockerfile
#

FROM jlesage/baseimage-gui:alpine-3.9-glibc

# Define environment variables
ENV RCLONE_INSTALL_VERSION=current
ENV ARCH=amd64

# Define working directory.
WORKDIR /tmp

# Install our hard link from our no_local repo
COPY ./scripts/rclone /usr/bin

# Install Rclone Browser dependencies

RUN echo "make sure you run cp ~/Development/Current/rclone/bin/rclone ~/Development/Current/rclonebrowser-app/scripts/"

RUN add-pkg \
      ca-certificates \
      fuse \
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
    && add-pkg --virtual=build-dependencies \
        build-base \
        cmake \
        make \
        gcc \
        git \
        qt5-qtbase qt5-qtmultimedia-dev qt5-qttools-dev && \

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
        /etc/xdg/openbox/rc.xml

# Generate and install favicons.
RUN \
    APP_ICON_URL=https://github.com/rclone/rclone/raw/master/graphics/logo/logo_symbol/logo_symbol_color_512px.png && \
    install_app_icon.sh "$APP_ICON_URL"

# Add files.
COPY rootfs/ /
COPY VERSION /

# Set environment variables.
ENV APP_NAME="RcloneBrowser" \
    S6_KILL_GRACETIME=8000

RUN mkdir /scripts
COPY scripts/ /scripts/

# Metadata.
LABEL \
      org.label-schema.name="rclonebrowser" \
      org.label-schema.description="Docker container for RcloneBrowser" \
      org.label-schema.version="unknown" \
      org.label-schema.vcs-url="https://github.com/romancin/rclonebrowser-docker" \
      org.label-schema.schema-version="1.0"
