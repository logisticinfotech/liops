# SPDX-License-Identifier: Apache-2.0

BINARY  := liops
VERSION ?= dev
LDFLAGS := -s -w -X github.com/logisticinfotech/liops/app/version.Version=$(VERSION)

.PHONY: build build-linux test lint vuln css css-check testenv integration integration-mysql vm-test-mysql testenv-down dev-up dev dev-down

# Tailwind CSS's standalone CLI builds the UI's stylesheet: no Node or npm. It's downloaded
# once into bin/ and checked against the release's sha256sums.txt.
TAILWIND_VERSION := 4.3.3
TAILWIND_PLATFORM := $(shell uname -s | sed 's/Darwin/macos/;s/Linux/linux/')-$(shell uname -m | sed 's/x86_64/x64/;s/aarch64/arm64/')
TAILWIND_SHA256_macos-arm64 := cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d
TAILWIND_SHA256_macos-x64   := 7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1
TAILWIND_SHA256_linux-x64   := dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
TAILWIND_SHA256_linux-arm64 := 55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
TAILWIND := bin/tailwindcss-$(TAILWIND_VERSION)-$(TAILWIND_PLATFORM)

build:            ## Build for this machine
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

build-linux:      ## Build the static linux/amd64 release binary into dist/
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 .

$(TAILWIND):
	@mkdir -p bin
	@sum="$(TAILWIND_SHA256_$(TAILWIND_PLATFORM))"; [ -n "$$sum" ] || { echo "no Tailwind build for $(TAILWIND_PLATFORM)"; exit 1; }; \
	curl -fsSL -o $@.tmp https://github.com/tailwindlabs/tailwindcss/releases/download/v$(TAILWIND_VERSION)/tailwindcss-$(TAILWIND_PLATFORM) && \
	echo "$$sum  $@.tmp" | shasum -a 256 -c --quiet - && chmod 755 $@.tmp && mv $@.tmp $@

css: $(TAILWIND) ## Build resources/static/app.css from resources/css/app.css and the templates
	$(TAILWIND) -i resources/css/app.css -o resources/static/app.css --minify

css-check: css    ## Fail if the committed app.css is out of date (CI)
	@git diff --quiet -- resources/static/app.css || { echo "resources/static/app.css is out of date: run make css and commit it"; exit 1; }

test:             ## Unit tests
	go test -race -count=1 ./...

lint:             ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (echo "Not gofmt'ed:"; gofmt -l .; exit 1)
	go vet ./...

vuln:             ## Check dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

testenv:          ## Start the Docker test environment and run the smoke test
	./test/smoke.sh

integration:      ## Setup, backups, dumps, emails and restores in the Docker test environment
	./test/integration.sh

integration-mysql: ## The same for MySQL 8.4: setup, dumps, users and grants, restores, emails
	./test/integration-mysql.sh

vm-test-mysql:    ## The MySQL integration test in a fresh Ubuntu VM (Multipass), no Docker
	./scripts/vm-test-mysql.sh

testenv-down:     ## Stop the test environment and delete its data
	docker compose -f test/compose.yaml --profile mysql down -v

dev-up:           ## Laptop dev environment without Docker: MinIO, Mailpit, dev/config.yaml (docs/development.md)
	./scripts/dev.sh up

dev: build        ## Serve the UI and scheduler against the dev environment
	LIOPS_DEV=1 ./$(BINARY) serve

dev-down:         ## Stop MinIO and Mailpit (dev/ is kept)
	./scripts/dev.sh down
