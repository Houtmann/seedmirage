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

# Front-end tooling: standalone Tailwind v4 executable (no Node), version and sha256 pinned (tools/).
tools:
	sh tools/fetch-tailwind.sh

# Generates internal/web/static/ui.css (committed) from input.css and the templates.
css:
	bin/tailwindcss -i internal/web/input.css -o internal/web/static/ui.css --minify

# ui.css must be up to date. Skipped (with a message) if bin/tailwindcss is missing, except in CI (CI=true).
css-check:
	@if [ ! -x bin/tailwindcss ]; then \
	  if [ "$$CI" = "true" ]; then echo "css-check: bin/tailwindcss missing (make tools)"; exit 1; fi; \
	  echo "css-check skipped: bin/tailwindcss missing (make tools)"; \
	else \
	  tmp=$$(mktemp); \
	  bin/tailwindcss -i internal/web/input.css -o "$$tmp" --minify >/dev/null 2>&1 \
	    && cmp -s "$$tmp" internal/web/static/ui.css; st=$$?; \
	  rm -f "$$tmp"; \
	  if [ $$st -ne 0 ]; then echo "css-check: ui.css is out of date, run make css"; exit 1; fi; \
	fi

# Local image (context = repository root). To run it: make up.
docker:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t seedmirage:dev .

# Root compose: builds the image from the sources, then starts it (see docs/docker.md).
up:
	@mkdir -p data
	docker compose up -d --build

down:
	docker compose down

# Multi-OS binaries (darwin arm64/amd64, linux amd64/arm64, windows amd64): archives + SHA256SUMS in dist/.
dist:
	sh tools/dist.sh $(VERSION)

# The repository is public: nothing personal or internal in tracked files (tools/public-check.sh).
public-check:
	sh tools/public-check.sh
