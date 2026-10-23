# PARENT is set by prebake to <registry>/base:latest.
ARG PARENT
FROM ${PARENT}

# Repeat the full package list and add what you miss. apt skips everything the
# base already has, so adding one tool costs seconds instead of a full rebuild.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git jq less make openssh-client procps ripgrep strace tcpdump vim-tiny \
      shellcheck \
 && rm -rf /var/lib/apt/lists/*
