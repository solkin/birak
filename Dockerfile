FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /birakd ./cmd/birakd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /birakd /usr/local/bin/birakd

# Run as a non-root user that owns the data directories. The SFTP host key is
# stored under /data/meta (a volume), so it persists across restarts instead of
# being regenerated (which would trip host-key-changed warnings on clients).
RUN addgroup -g 1000 birak && adduser -D -u 1000 -G birak birak \
    && mkdir -p /data/sync /data/meta \
    && chown -R birak:birak /data

VOLUME ["/data/sync", "/data/meta"]
USER birak
EXPOSE 9100 9200 9300 9400 9500
ENV BIRAK_SYNC_DIR="/data/sync" \
    BIRAK_META_DIR="/data/meta"
ENTRYPOINT ["birakd"]
