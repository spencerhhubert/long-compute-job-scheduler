GO ?= go

.PHONY: build check fmt-check test vet

build:
	$(GO) build ./...

fmt-check:
	@test -z "$$(gofmt -l .)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

check: fmt-check test vet build
