# syntax=docker/dockerfile:1
# Go image: multi-stage, native cross-compile via TARGETOS/TARGETARCH (no QEMU
# emulation), ~15-25MB final image. HEALTHCHECK uses the binary's -healthcheck
# flag mode (distroless has no shell/wget).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/proxy /proxy
EXPOSE 8318
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["-healthcheck"]
ENTRYPOINT ["/proxy"]
