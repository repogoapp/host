# Builds the tunnel gateway only. Context is apps/host: fly deploy -c tunnelgateway/fly.toml
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
# Only the gateway's own packages, so importing other host code fails the build.
COPY tunnelwire tunnelwire
COPY internal/tunnelgateway internal/tunnelgateway
COPY tunnelgateway tunnelgateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tunnelgateway ./tunnelgateway

FROM alpine:3.21
# TLS roots for Upstash, and a non-root user because nothing here needs privileges.
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 gateway
USER gateway
COPY --from=build /out/tunnelgateway /usr/local/bin/tunnelgateway
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/tunnelgateway"]
