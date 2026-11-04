GO ?= go

.PHONY: build test fmt vet check
build:
	$(GO) build -o bin/solarguard ./cmd/solarguard
test:
	$(GO) test -race ./...
fmt:
	$(GO) fmt ./...
vet:
	$(GO) vet ./...
check: vet test
