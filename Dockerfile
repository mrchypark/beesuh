# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/beesuh ./cmd/beesuh

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 beesuh \
    && adduser -D -u 10001 -G beesuh beesuh \
    && mkdir -p /app /data \
    && chown beesuh:beesuh /app /data

COPY --from=build /out/beesuh /usr/local/bin/beesuh
USER beesuh
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["beesuh"]
CMD ["-listen", "0.0.0.0:8080", "-data-dir", "/data"]
