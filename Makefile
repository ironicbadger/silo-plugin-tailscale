.PHONY: prepare build test vet check dist
VERSION ?= 0.1.0
GOFLAGS := -modfile=$(CURDIR)/.build/go.mod
export GOFLAGS

prepare:
	GOFLAGS= python3 scripts/prepare_tailscale.py

build: prepare
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o plugin .

test: prepare
	go test -race ./...
	go test -race tailscale.com/feature/acme -run '^TestSiloCustomStateStore'

vet: prepare
	go vet ./...

check: test vet
	test -z "$$(gofmt -l main.go internal)"

dist: prepare
	python3 scripts/dist.py $(VERSION)
