FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /birakd ./cmd/birakd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /birakd /usr/local/bin/birakd
VOLUME ["/data/sync", "/data/meta"]
EXPOSE 9100 9200 9300 9400
ENTRYPOINT ["birakd"]
CMD ["-config", "/etc/birak/config.yaml"]
