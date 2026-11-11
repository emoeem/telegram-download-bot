# syntax=docker/dockerfile:1.7

# Build stage: the toolchain never reaches the runtime image.
# Must match the `go` directive in go.mod / go.work (1.26.0). A 1.25 toolchain
# cannot build a module that declares 1.26.
FROM golang:1.26-alpine AS build

ARG VERSION="dev"
ARG COMMIT="unknown"
ARG COMMIT_DATE="unknown"
ARG TARGETOS
ARG TARGETARCH

# 授权校验材料。留空 = 不注入（源码构建的正常状态，容器里不强制授权）。
# 发行镜像必须注入，否则容器内的软件没有授权门：
#   ./tdl license verifier --key-file ~/.tdl-issuer.txt
# 然后
#   docker build --build-arg LICENSE_HMAC=<hmacKeyB64> --build-arg LICENSE_PUB=<pubKeyB64> .
ARG LICENSE_HMAC=""
ARG LICENSE_PUB=""

# 在线授权服务地址。留空 = 只能离线激活（客户要自己填服务器地址）。
# 发行镜像应注入，客户首次激活会直接连这里，不需要任何命令行参数：
#   --build-arg LICENSE_SERVER=https://lic.example.com
ARG LICENSE_SERVER=""

WORKDIR /src

COPY go.mod go.sum go.work go.work.sum ./
COPY core ./core
COPY extension ./extension
COPY app ./app
COPY archive ./archive
COPY cmd ./cmd
COPY pkg ./pkg
COPY license ./license
COPY main.go ./

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOMAXPROCS=2 \
    go build -p=1 -trimpath \
    -ldflags "-s -w \
    -X github.com/iyear/tdl/pkg/consts.Version=${VERSION} \
    -X github.com/iyear/tdl/pkg/consts.Commit=${COMMIT} \
    -X github.com/iyear/tdl/pkg/consts.CommitDate=${COMMIT_DATE} \
    -X github.com/iyear/tdl/license.hmacKeyB64=${LICENSE_HMAC} \
    -X github.com/iyear/tdl/license.pubKeyB64=${LICENSE_PUB} \
    -X github.com/iyear/tdl/archive.licenceServerDefault=${LICENSE_SERVER}" \
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
