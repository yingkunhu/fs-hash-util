.PHONY: all build-all build-darwin-arm64 build-linux-amd64 build-linux-arm64 test lint clean

VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

all: build-all

build-all: build-darwin-arm64 build-linux-amd64 build-linux-arm64

build-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/fshash-darwin-arm64 .

build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/fshash-linux-amd64 .

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/fshash-linux-arm64 .

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fshash .

test:
	CGO_ENABLED=0 go test ./...

lint:
	golangci-lint run

clean:
	rm -rf dist/
