# SSXNG — build and test entry points.
SHELL := /bin/bash
APP_NAME := SSXNG
APP_DIR := build/bin/$(APP_NAME).app
HELPER := internal/helperbin/bin/ssxng-helper

.PHONY: all build helper dev test vet fmt tidy run clean sign uninstall ui-check tray-check

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

## ui-check: headless UI regression check (no npm dependencies)
ui-check:
	scripts/ui-check.sh

## tray-check: headless check of the Cocoa menu bar items
tray-check:
	scripts/tray-check.sh

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

## clean: drop build output, the embedded helper and the local build caches
clean:
	rm -rf build/bin $(HELPER) .cache .gocache .gotmp build/ui-check

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
