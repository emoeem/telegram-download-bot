# syntax=docker/dockerfile:1.7

# Build stage: the toolchain never reaches the runtime image.
FROM golang:1.25-alpine AS build

ARG VERSION="dev"
ARG COMMIT="unknown"
ARG COMMIT_DATE="unknown"
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum go.work go.work.sum ./
COPY core ./core
COPY extension ./extension
COPY app ./app
COPY archive ./archive
COPY cmd ./cmd
COPY pkg ./pkg
COPY main.go ./

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOMAXPROCS=2 \
    go build -p=1 -trimpath \
    -ldflags "-s -w \
    -X github.com/iyear/tdl/pkg/consts.Version=${VERSION} \
    -X github.com/iyear/tdl/pkg/consts.Commit=${COMMIT} \
    -X github.com/iyear/tdl/pkg/consts.CommitDate=${COMMIT_DATE}" \
    -o /out/telegram-download-bot .

FROM alpine:3.22

# ca-certificates for Telegram/webhooks, tzdata for archive timestamps.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 tdl \
    && adduser -S -u 10001 -G tdl tdl \
    && mkdir -p /data \
    && chown -R tdl:tdl /data

COPY --from=build /out/telegram-download-bot /usr/local/bin/telegram-download-bot

WORKDIR /app
ENV HOME=/data
VOLUME ["/data"]
USER tdl

ENTRYPOINT ["telegram-download-bot"]
