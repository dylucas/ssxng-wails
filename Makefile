# SSXNG — build and test entry points.
SHELL := /bin/bash
APP_NAME := SSXNG
APP_DIR := build/bin/$(APP_NAME).app
HELPER := internal/helperbin/bin/ssxng-helper

.PHONY: all build helper dev test test-race vet fmt tidy run clean sign uninstall ui-check tray-check

# -race 用一份独立的构建缓存，和普通构建分开。
#
# 症状与原因：`go test -race` 有时会以
#   runtime/race: package testmain: cannot find package
# （或某个缓存文件 `operation not permitted`）失败，而重新构建一遍就没事。
# 这是构建缓存的问题，不是代码问题；-race 的产物和普通产物挤在同一个缓存里时
# 最容易撞上。独立目录让这条命令可复现，也让 `go clean -cache` 不再是排障前置。
#
# 需要指定别的缓存（例如缓存目录不可写）时：make test-race RACE_CACHE=/tmp/x
RACE_CACHE ?= $(CURDIR)/.gocache-race

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

## test-race: unit tests under the race detector (independent build cache, see RACE_CACHE)
test-race:
	GOCACHE=$(RACE_CACHE) go test -race ./...

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
	rm -rf build/bin $(HELPER) .cache .gocache .gocache-race .gotmp build/ui-check

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
