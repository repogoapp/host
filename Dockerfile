# Build the relay only. It is the one binary in this module that is safe to run
# on our infrastructure: no agents, no filesystem, no database.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, so a code change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static and stripped; the build stamp comes from FLY_MACHINE_VERSION at run time.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/relay ./relay

FROM alpine:3.21
# TLS roots, and a non-root user because nothing here needs privileges.
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 relay
USER relay

COPY --from=build /out/relay /usr/local/bin/relay

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/relay"]
