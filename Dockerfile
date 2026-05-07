# Multi-stage build for the devenv-manager server image.
#
# Stage 1 builds a static binary against Go 1.22; stage 2 ships only the
# binary + minimal CA certificates so the runtime image stays small.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Module cache layer.
COPY go.mod go.sum ./
RUN go mod download

# Sources.
COPY pkg pkg
COPY internal internal
COPY cmd cmd

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/reaper ./cmd/reaper

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 devenv
USER devenv:devenv
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/reaper /usr/local/bin/reaper
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
