BINARY  := ramplus
DAEMON  := ramplusd
WEB     := ramplus-web
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Install (sudo make install). systemd also looks for units, sysusers.d and
# tmpfiles.d under /usr/local/lib/...; DESTDIR is for staging. Distribution
# packages (.deb) are built by GoReleaser with /usr paths: make deb.
PREFIX      ?= /usr/local
BINDIR      ?= $(PREFIX)/bin
UNITDIR     ?= $(PREFIX)/lib/systemd/system
SYSUSERSDIR ?= $(PREFIX)/lib/sysusers.d
TMPFILESDIR ?= $(PREFIX)/lib/tmpfiles.d
DESTDIR     ?=

GORELEASER ?= goreleaser
LINTIAN_IMAGE ?= ubuntu:24.04

.PHONY: all build build-probe web web-test test lint fmt clean install uninstall deb lintian release-check

all: lint test web-test build build-probe

# Frontend (web/, React + Vite) → internal/webui/dist, embedded into
# ramplus-web via go:embed.
web:
	cd web && npm ci --no-audit --no-fund && npm run build

web-test:
	cd web && npm ci --no-audit --no-fund && npm test

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/ramplus
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DAEMON) ./cmd/ramplusd
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(WEB) ./cmd/ramplus-web

build-probe:
	CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o ramplus-probe ./cmd/ramplus-probe

test:
	go test ./...

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v shellcheck >/dev/null; then shellcheck scripts/*.sh packaging/deb/postinst packaging/deb/prerm packaging/deb/postrm packaging/deb/prepare.sh; else echo "shellcheck not installed — skipped (CI runs it)"; fi

fmt:
	gofmt -w .

clean:
	rm -f $(BINARY) $(DAEMON) $(WEB) ramplus-probe
	rm -rf dist build

# Local .deb and tar.gz in dist/ (snapshot: no tag needed, nothing published).
deb:
	$(GORELEASER) release --snapshot --clean --skip=publish
	@ls -1 dist/*.deb

# lintian for the .deb from make deb, in a clean Ubuntu container (docker).
lintian:
	docker run --rm -v "$(CURDIR)/dist:/dist:ro" $(LINTIAN_IMAGE) sh -c '\
		apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends lintian >/dev/null && \
		lintian --fail-on error --pedantic /dist/*.deb'

release-check:
	$(GORELEASER) check

# install does not build (go may be missing under sudo): run make build first.
install:
	@for b in $(BINARY) $(DAEMON) $(WEB); do [ -x $$b ] || { echo "no ./$$b — run make build first"; exit 1; }; done
	install -Dm755 $(BINARY) $(DESTDIR)$(BINDIR)/$(BINARY)
	install -Dm755 $(DAEMON) $(DESTDIR)$(BINDIR)/$(DAEMON)
	install -Dm755 $(WEB) $(DESTDIR)$(BINDIR)/$(WEB)
	install -d $(DESTDIR)$(UNITDIR)
	for u in ramplusd ramplus-web; do \
		sed 's|@BINDIR@|$(BINDIR)|g' packaging/systemd/$$u.service > $(DESTDIR)$(UNITDIR)/$$u.service; \
		chmod 644 $(DESTDIR)$(UNITDIR)/$$u.service; \
	done
	install -Dm644 packaging/sysusers.d/ramplus.conf $(DESTDIR)$(SYSUSERSDIR)/ramplus.conf
	install -Dm644 packaging/tmpfiles.d/ramplus.conf $(DESTDIR)$(TMPFILESDIR)/ramplus.conf
	@if [ -z "$(DESTDIR)" ]; then \
		set -e; \
		systemd-sysusers $(SYSUSERSDIR)/ramplus.conf; \
		systemd-tmpfiles --create $(TMPFILESDIR)/ramplus.conf; \
		systemctl daemon-reload; \
		echo "installed; daemon: sudo systemctl enable --now ramplusd"; \
		echo "web: sudo ramplus init && sudo systemctl enable --now ramplus-web → http://127.0.0.1:7781"; \
		echo "daemon access from the CLI: sudo usermod -aG ramplus USER (read), -aG ramplus,ramplus-admin (changes)"; \
	fi

# The journal /var/lib/ramplus is kept: rollback still works after reinstalling.
# Refused while RamPlus still manages swap/a profile (or left traces without a
# journal): swap remove / profile reset / swap remove --orphan first; FORCE=1
# removes anyway. Groups ramplus, ramplus-admin, the ramplus-web user and the
# web password (/etc/ramplus/web-auth) are removed only with PURGE=1.
uninstall:
	@if [ -z "$(DESTDIR)" ] && [ "$(FORCE)" != 1 ]; then \
		rp=$(BINDIR)/$(BINARY); [ -x $$rp ] || rp=./$(BINARY); \
		if [ -x $$rp ] && ! LC_ALL=C.UTF-8 $$rp swap check-clean; then \
			echo; \
			echo "uninstall refused: RamPlus still manages this system (see above)."; \
			echo "First: sudo ramplus swap remove and/or sudo ramplus profile reset"; \
			echo "(traces without a journal entry: sudo ramplus swap remove --orphan)."; \
			echo "Remove anyway, keeping swap/profile as they are: sudo make uninstall FORCE=1"; \
			exit 1; \
		fi; \
	fi
	@if [ -z "$(DESTDIR)" ]; then \
		for u in ramplus-web ramplusd; do \
			systemctl cat $$u.service >/dev/null 2>&1 || continue; \
			systemctl stop $$u.service; \
			systemctl disable $$u.service; \
			if systemctl is-active --quiet $$u.service; then echo "$$u did not stop"; exit 1; fi; \
			echo "$$u stopped and disabled"; \
		done; \
	fi
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY) $(DESTDIR)$(BINDIR)/$(DAEMON) $(DESTDIR)$(BINDIR)/$(WEB)
	rm -f $(DESTDIR)$(UNITDIR)/ramplusd.service $(DESTDIR)$(UNITDIR)/ramplus-web.service
	rm -f $(DESTDIR)$(SYSUSERSDIR)/ramplus.conf $(DESTDIR)$(TMPFILESDIR)/ramplus.conf
	@if [ -z "$(DESTDIR)" ]; then \
		systemctl daemon-reload; \
		rm -rf /run/ramplus; \
		if [ "$(PURGE)" = 1 ]; then \
			echo "PURGE=1: removing the web password, the ramplus-web user and groups ramplus, ramplus-admin — their members lose membership"; \
			rm -f /etc/ramplus/web-auth; rmdir /etc/ramplus 2>/dev/null || true; \
			if getent passwd ramplus-web >/dev/null; then userdel ramplus-web && echo "user ramplus-web removed"; fi; \
			for g in ramplus-web ramplus-admin ramplus; do if getent group $$g >/dev/null; then groupdel $$g && echo "group $$g removed"; fi; done; \
		else \
			echo "kept: user ramplus-web, groups ramplus and ramplus-admin, /etc/ramplus/web-auth (remove: sudo make uninstall PURGE=1)"; \
		fi; \
	fi
