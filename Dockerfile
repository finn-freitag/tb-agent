# tb-agent image for Linux nodes.
# The agent talks to host services through mounted sockets (libvirt, Docker,
# Incus) and forwards device ports, so it runs with host networking.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/finn-freitag/tb-agent/internal/version.Version=${VERSION}" \
    -o /out/tb-agent ./cmd/tb-agent

FROM debian:trixie-slim
# adb/fastboot/usbutils: android + usb-phone drivers; qemu-utils: image inspection.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates qemu-utils adb fastboot usbutils \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/tb-agent /usr/local/bin/tb-agent
VOLUME /var/lib/tb-agent
ENV TB_AGENT_CONFIG=/etc/tb-agent/node.yaml
ENTRYPOINT ["/usr/local/bin/tb-agent"]
CMD ["run"]
