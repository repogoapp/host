# Builds hrelay only. Context is apps/host: fly deploy -c hrelay/fly.toml
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hrelay ./hrelay

FROM alpine:3.21
RUN apk add --no-cache ca-certificates su-exec \
 && adduser -D -u 10001 relay
COPY --from=build /out/hrelay /usr/local/bin/hrelay
EXPOSE 8443
# The volume mounts root-owned; hand the cache to the relay user, then drop root.
ENTRYPOINT ["/bin/sh", "-c", "mkdir -p /data/acme && chown -R relay /data && exec su-exec relay /usr/local/bin/hrelay"]
