FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/envpilot ./apps/api

FROM alpine:3.21

RUN apk add --no-cache ca-certificates helm kubectl && \
    addgroup -S -g 10001 envpilot && \
    adduser -S -D -H -u 10001 -G envpilot envpilot
COPY --from=builder /out/envpilot /usr/local/bin/envpilot
USER 10001:10001
ENTRYPOINT ["envpilot"]
