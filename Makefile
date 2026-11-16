BINARY  := ramplus
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-probe test lint fmt clean

all: lint test build build-probe

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/ramplus

build-probe:
	CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o ramplus-probe ./cmd/ramplus-probe

test:
	go test ./...

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v shellcheck >/dev/null; then shellcheck scripts/*.sh; else echo "shellcheck не установлен — пропуск (проверяется в CI)"; fi

fmt:
	gofmt -w .

clean:
	rm -f $(BINARY) ramplus-probe
