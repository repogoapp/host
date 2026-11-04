# Builds bench/flysni only. Context is apps/host.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/flysni ./bench/flysni

FROM alpine:3.21
RUN adduser -D -u 10001 probe
USER probe
COPY --from=build /out/flysni /usr/local/bin/flysni
ENTRYPOINT ["/usr/local/bin/flysni"]
