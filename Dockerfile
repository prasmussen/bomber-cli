FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bomber-cli ./cmd/bomber-cli

FROM alpine:3.22
RUN addgroup -g 10001 bomber && adduser -D -u 10001 -G bomber bomber \
    && mkdir /data && chown bomber:bomber /data
COPY --from=build /bomber-cli /usr/local/bin/bomber-cli
USER 10001:10001
WORKDIR /data
EXPOSE 2323
VOLUME ["/data"]
ENTRYPOINT ["bomber-cli"]
CMD ["--host-key", "/data/ssh_host_ed25519_key"]
