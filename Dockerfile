# syntax=docker/dockerfile:1
#
# Layer order matters for CI: everything that depends on the version or commit (ARG, LABEL,
# ENV) comes last, so the expensive layers (module download, compile, apt) are identical for
# every version of the same sources and come from the layer cache.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/prebake ./cmd/prebake

FROM docker:cli AS dockercli

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git openssh-client tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=build /out/prebake /usr/local/bin/prebake
ENV DATA_DIR=/data
VOLUME /data
EXPOSE 5000 5001 8080
ENTRYPOINT ["prebake"]
CMD ["daemon"]

# the version is not compiled in (that would change the binary and with it every layer above):
# the binary reads these two variables when it was built without -ldflags
ARG VERSION=dev
ARG REVISION=unknown
ENV PREBAKE_VERSION=${VERSION} PREBAKE_REVISION=${REVISION}
LABEL org.opencontainers.image.title="prebake" \
      org.opencontainers.image.description="Keeps base images for dev containers built and fresh, served from a read-only registry" \
      org.opencontainers.image.source="https://github.com/schorsch3000/prebake" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
