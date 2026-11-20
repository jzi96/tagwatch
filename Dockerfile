# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tagwatch ./cmd/tagwatch

FROM alpine:3.24
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 tagwatch \
    && adduser -S -D -H -u 10001 -G tagwatch tagwatch \
    && mkdir /data \
    && chown tagwatch:tagwatch /data
COPY --from=build /out/tagwatch /usr/local/bin/tagwatch
COPY config.yaml /etc/tagwatch/config.yaml
USER 10001:10001
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/tagwatch"]
CMD ["-config", "/etc/tagwatch/config.yaml"]
