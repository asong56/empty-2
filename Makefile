# TardiTalk — Build & Run
# Usage:
#   make build     → compile optimised binary (~20% smaller)
#   make dev       → run with live reload
#   make clean

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo "dev")

.PHONY: build dev clean

build:
	CGO_ENABLED=0 go build \
		-ldflags="-s -w -X main.AppVersion=$(VERSION)" \
		-trimpath \
		-o tarditalk \
		.

dev:
	TARDI_ADDR=:8888 go run .

clean:
	rm -f tarditalk
