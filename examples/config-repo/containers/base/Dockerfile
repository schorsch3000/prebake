FROM debian:trixie-slim

# The big package list lives in one layer of the base image. prebake rebuilds
# this image weekly and refreshes it daily, so most packages are already here
# when a derived image asks for them.
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git jq less make openssh-client procps ripgrep strace tcpdump vim-tiny \
 && rm -rf /var/lib/apt/lists/*
