# seedmirage: local image (build from the repository root: make docker).
FROM golang:1.26 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY go.su[m] ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/seedmirage ./cmd/seedmirage \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/seedmirage /seedmirage
# Empty data directory, writable by the nonroot user (named volume or bind mount).
COPY --from=build --chown=65532:65532 /out/data /data
# The container is isolated: real exposure depends on the published port (127.0.0.1 in compose.yaml).
ENV SEEDMIRAGE_LISTEN=0.0.0.0:7070 \
    SEEDMIRAGE_IN_CONTAINER=1 \
    SEEDMIRAGE_IN_DOCKER=1 \
    SEEDMIRAGE_LOG_FORMAT=json
VOLUME /data
EXPOSE 7070
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/seedmirage", "healthcheck"]
ENTRYPOINT ["/seedmirage", "serve", "--data-dir", "/data"]
