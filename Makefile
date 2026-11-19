VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
DATA_DIR ?= ./data

.PHONY: build run test lint fmt fmt-check vet check tools css css-check docker up down dist public-check

build:
	go build -ldflags "$(LDFLAGS)" -o bin/seedmirage ./cmd/seedmirage

run: build
	./bin/seedmirage serve --data-dir $(DATA_DIR)

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w cmd internal

fmt-check:
	@out=$$(gofmt -l cmd internal 2>/dev/null); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

check: fmt-check vet lint test css-check build public-check

# Outillage front : exécutable autonome Tailwind v4 (sans Node), version et sha256 épinglés (tools/).
tools:
	sh tools/fetch-tailwind.sh

# Génère internal/web/static/ui.css (commité) à partir d'input.css et des templates.
css:
	bin/tailwindcss -i internal/web/input.css -o internal/web/static/ui.css --minify

# ui.css doit être à jour. Sauté (avec message) si bin/tailwindcss est absent, sauf en CI (CI=true).
css-check:
	@if [ ! -x bin/tailwindcss ]; then \
	  if [ "$$CI" = "true" ]; then echo "css-check: bin/tailwindcss absent (make tools)"; exit 1; fi; \
	  echo "css-check sauté : bin/tailwindcss absent (make tools)"; \
	else \
	  tmp=$$(mktemp); \
	  bin/tailwindcss -i internal/web/input.css -o "$$tmp" --minify >/dev/null 2>&1 \
	    && cmp -s "$$tmp" internal/web/static/ui.css; st=$$?; \
	  rm -f "$$tmp"; \
	  if [ $$st -ne 0 ]; then echo "css-check : ui.css n'est pas à jour, lancer make css"; exit 1; fi; \
	fi

# Image locale (contexte = racine du dépôt). Pour lancer : make up.
docker:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t seedmirage:dev .

# Compose racine : construit l'image depuis les sources puis la lance (voir docs/docker.md).
up:
	@mkdir -p data
	docker compose up -d --build

down:
	docker compose down

# Binaires multi-OS (darwin arm64/amd64, linux amd64/arm64, windows amd64) : archives + SHA256SUMS dans dist/.
dist:
	sh tools/dist.sh $(VERSION)

# Le dépôt est public : rien de personnel ni d'interne dans les fichiers suivis (tools/public-check.sh).
public-check:
	sh tools/public-check.sh
