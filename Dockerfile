FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sluice ./cmd/sluice

FROM alpine:3.21
# git + git-filter-repo are the actual engine (spec §3); gitleaks is an
# optional advisory scanner picked up automatically if present. su-exec drops
# privileges in the entrypoint; shadow provides usermod/groupmod for PUID/PGID.
RUN apk add --no-cache git git-filter-repo openssh-client ca-certificates tzdata su-exec shadow \
    && addgroup -g 1000 sluice \
    && adduser -D -h /home/sluice -u 1000 -G sluice sluice \
    && mkdir -p /data
COPY --from=build /out/sluice /usr/local/bin/sluice
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
ENV SLUICE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
# Starts as root so the entrypoint can align ownership, then drops to PUID/PGID
# (default 1000:1000; set PUID=99 PGID=100 on unraid).
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["sluice"]
