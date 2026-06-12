FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sluice ./cmd/sluice

FROM alpine:3.21
# git + git-filter-repo are the actual engine (spec §3); gitleaks is an
# optional advisory scanner picked up automatically if present.
RUN apk add --no-cache git git-filter-repo openssh-client ca-certificates tzdata \
    && adduser -D -u 1000 sluice \
    && mkdir -p /data && chown sluice:sluice /data
COPY --from=build /out/sluice /usr/local/bin/sluice
USER sluice
ENV SLUICE_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["sluice"]
