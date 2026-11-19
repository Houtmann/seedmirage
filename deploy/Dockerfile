# seedmirage : image locale (build depuis la racine du dépôt : make docker).
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
# Dossier de données vide, lisible par l'utilisateur nonroot (volume nommé ou bind mount).
COPY --from=build --chown=65532:65532 /out/data /data
# Le conteneur est isolé : l'exposition réelle dépend du port publié (127.0.0.1 dans docker-compose.yml).
ENV SEEDMIRAGE_LISTEN=0.0.0.0:7070 \
    SEEDMIRAGE_IN_CONTAINER=1 \
    SEEDMIRAGE_LOG_FORMAT=json
VOLUME /data
EXPOSE 7070
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/seedmirage", "healthcheck"]
ENTRYPOINT ["/seedmirage", "serve", "--data-dir", "/data"]
