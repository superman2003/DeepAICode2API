# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=docker" \
    -o /out/deepaicode2api .

FROM alpine:3.20

# hadolint ignore=DL3018
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app \
    && mkdir -p /data \
    && chown 10001:10001 /data

COPY --from=build /out/deepaicode2api /usr/local/bin/deepaicode2api

WORKDIR /app
USER 10001:10001

ENV DEEPAI2API_HOST=0.0.0.0 \
    DEEPAI2API_PORT=10087 \
    DEEPAI2API_DATA_PATH=/data/deepaicode2api-data.json

VOLUME ["/data"]
EXPOSE 10087

ENTRYPOINT ["deepaicode2api"]
