 .PHONY: build install clean test integration modverify modtidy release

GO    := GO15VENDOREXPERIMENT=1 go
VERSION=`egrep -o '[0-9]+\.[0-9a-z.\-]+' version.go`
GIT_SHA=`git rev-parse --short HEAD || echo`
pkgs   = $(shell $(GO) list ./... | grep -v /vendor/)


build:
	@echo "Building http_proxy..."
	@mkdir -p bin
	@go build -ldflags "-X main.GitSHA=${GIT_SHA}" -o bin/http_proxy .

build-static:
	@echo "Building static http_proxy..."
	@mkdir -p bin
	@CGO_ENABLED=0 go build -ldflags "-extldflags=-static -X main.GitSHA=${GIT_SHA}" -o bin/http_proxy .

install:
	@echo "Installing http_proxy..."
	@install -c bin/http_proxy /usr/local/bin/http_proxy

format:
	@echo "Formatting code ..."
	@$(GO) fmt $(pkgs)

clean:
	@rm -f bin/*

test:
	@echo "Running tests..."
	@go test `go list ./... | grep -v vendor/`

integration: modtidy build test
	@echo "Running integration tests..."
	#bash integration/run.sh
	@docker run -it --rm -v $(pwd):/go/src/github.com/haad/http_proxy golang:1.20.3 /go/src/github.com/haad/http_proxy/integration/run.sh

modtidy:
	@go mod tidy

modverify:
	@go mod verify
