BINARY  := ramplus
DAEMON  := ramplusd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Установка (sudo make install). systemd ищет юниты, sysusers.d и tmpfiles.d
# и в /usr/local/lib/...; DESTDIR — для сборки пакетов.
PREFIX      ?= /usr/local
BINDIR      ?= $(PREFIX)/bin
UNITDIR     ?= $(PREFIX)/lib/systemd/system
SYSUSERSDIR ?= $(PREFIX)/lib/sysusers.d
TMPFILESDIR ?= $(PREFIX)/lib/tmpfiles.d
DESTDIR     ?=

.PHONY: all build build-probe test lint fmt clean install uninstall

all: lint test build build-probe

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/ramplus
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DAEMON) ./cmd/ramplusd

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
	rm -f $(BINARY) $(DAEMON) ramplus-probe

# install не собирает (под sudo может не быть go): сначала make build.
install:
	@for b in $(BINARY) $(DAEMON); do [ -x $$b ] || { echo "нет ./$$b — сначала make build"; exit 1; }; done
	install -Dm755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	install -Dm755 $(DAEMON) $(DESTDIR)$(BINDIR)/$(DAEMON)
	install -d $(DESTDIR)$(UNITDIR)
	sed 's|@BINDIR@|$(BINDIR)|g' packaging/systemd/ramplusd.service > $(DESTDIR)$(UNITDIR)/ramplusd.service
	chmod 644 $(DESTDIR)$(UNITDIR)/ramplusd.service
	install -Dm644 packaging/sysusers.d/ramplus.conf $(DESTDIR)$(SYSUSERSDIR)/ramplus.conf
	install -Dm644 packaging/tmpfiles.d/ramplus.conf $(DESTDIR)$(TMPFILESDIR)/ramplus.conf
	@if [ -z "$(DESTDIR)" ]; then \
		set -e; \
		systemd-sysusers $(SYSUSERSDIR)/ramplus.conf; \
		systemd-tmpfiles --create $(TMPFILESDIR)/ramplus.conf; \
		systemctl daemon-reload; \
		echo "установлено; запустить: sudo systemctl enable --now ramplusd"; \
		echo "доступ: sudo usermod -aG ramplus USER (чтение), -aG ramplus,ramplus-admin (изменения)"; \
	fi

# Журнал /var/lib/ramplus не удаляется: rollback возможен и после переустановки.
uninstall:
	@if [ -z "$(DESTDIR)" ]; then systemctl disable --now ramplusd.service 2>/dev/null || true; fi
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY) $(DESTDIR)$(BINDIR)/$(DAEMON)
	rm -f $(DESTDIR)$(UNITDIR)/ramplusd.service
	rm -f $(DESTDIR)$(SYSUSERSDIR)/ramplus.conf $(DESTDIR)$(TMPFILESDIR)/ramplus.conf
	@if [ -z "$(DESTDIR)" ]; then \
		systemctl daemon-reload; \
		rm -rf /run/ramplus; \
		for g in ramplus-admin ramplus; do if getent group $$g >/dev/null; then groupdel $$g; fi; done; \
	fi
