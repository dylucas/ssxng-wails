# SSXNG — build and test entry points.
SHELL := /bin/bash
APP_NAME := SSXNG
APP_DIR := build/bin/$(APP_NAME).app
HELPER := internal/helperbin/bin/ssxng-helper

.PHONY: all build helper dev test vet fmt tidy app run clean sign uninstall

all: build

## build: helper + app bundle (release)
build:
	scripts/build.sh

## helper: build and ad-hoc sign the privileged helper only
helper:
	mkdir -p internal/helperbin/bin
	CGO_ENABLED=0 go build -trimpath -o $(HELPER) ./cmd/ssxng-helper
	codesign --force --sign - --identifier org.ssxng.ssxng-helper $(HELPER)
	$(HELPER) --version

## dev: wails dev with the helper rebuilt first
dev:
	scripts/dev.sh

## test: unit tests (no privileged operations)
test:
	go test ./...

## vet: static analysis
vet:
	go vet ./...

## fmt: format all Go sources
fmt:
	gofmt -w .

## tidy: refresh go.mod/go.sum
tidy:
	go mod tidy

## run: build and launch the app bundle
run: build
	open $(APP_DIR)

## sign: ad-hoc sign everything inside the bundle
sign:
	codesign --force --sign - --identifier org.ssxng.SSXNG $(APP_DIR)
	codesign --verify --deep --strict $(APP_DIR)

## uninstall: remove the helper, the login item (and the config with PURGE=1)
uninstall:
	scripts/uninstall.sh $(if $(PURGE),--purge-config,)

## clean: drop build output and the local build cache
clean:
	rm -rf build/bin $(HELPER) .cache

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
