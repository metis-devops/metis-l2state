# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine AS build
WORKDIR /app
RUN apk add --no-cache ca-certificates git
COPY . ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o ./bin/ ./cmd/...

FROM alpine:latest AS runtime
RUN apk add --no-cache ca-certificates
COPY --from=build /app/bin/ /usr/local/bin/

FROM runtime AS ovm-event-sync
ENTRYPOINT ["/usr/local/bin/ovm-event-sync"]

FROM runtime AS l2state
COPY ./docs/ovm-reports/ /ovm-reports/
ENTRYPOINT ["/usr/local/bin/l2state"]
