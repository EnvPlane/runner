# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/envpilot ./apps/api

FROM alpine:3.21

RUN apk add --no-cache ca-certificates helm kubectl && \
    addgroup -S -g 10001 envpilot && \
    adduser -S -D -H -u 10001 -G envpilot envpilot
COPY --from=builder /out/envpilot /usr/local/bin/envpilot
USER 10001:10001
ENTRYPOINT ["envpilot"]
