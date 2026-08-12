ARG GO_VERSION=1.26.5

FROM golang:${GO_VERSION}-alpine AS helper-build
ARG TARGETARCH
WORKDIR /src/e2b-local
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/sbx-init ./internal/backends/sbx/cmd/sbx-init
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/sbx-tunnel ./internal/backends/sbx/cmd/sbx-tunnel

FROM alpine:3.22
ARG TARGETARCH
LABEL org.opencontainers.image.title="e2b-local sbx base" \
      org.opencontainers.image.description="Reusable Docker Sandboxes base image with envd bootstrap helpers"
COPY envd-bin/envd-linux-${TARGETARCH} /usr/local/bin/envd
COPY --from=helper-build /out/sbx-init /usr/local/bin/sbx-init
COPY --from=helper-build /out/sbx-tunnel /usr/local/bin/sbx-tunnel
RUN mkdir -p /var/log /tmp /home/oai/share
EXPOSE 49983
ENTRYPOINT ["/usr/local/bin/sbx-init"]
