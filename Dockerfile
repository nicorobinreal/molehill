# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/molehill .

# Needs ip (iproute2, busybox ip lacks policy routing bits) and wg. Runs as
# root: creating interfaces and routes needs NET_ADMIN in the container.
FROM alpine:3.22
RUN apk add --no-cache iproute2 wireguard-tools-wg ca-certificates
COPY --from=build /out/molehill /usr/local/bin/molehill
VOLUME /data
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/molehill"]
