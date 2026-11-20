BINARY  := ramplus
DAEMON  := ramplusd
WEB     := ramplus-web
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

.PHONY: all build build-probe web web-test test lint fmt clean install uninstall

all: lint test web-test build build-probe

# Фронтенд (web/, React + Vite) → internal/webui/dist, встраивается в
# ramplus-web через go:embed.
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
	@if command -v shellcheck >/dev/null; then shellcheck scripts/*.sh; else echo "shellcheck не установлен — пропуск (проверяется в CI)"; fi

fmt:
	gofmt -w .

clean:
	rm -f $(BINARY) $(DAEMON) $(WEB) ramplus-probe

# install не собирает (под sudo может не быть go): сначала make build.
install:
	@for b in $(BINARY) $(DAEMON) $(WEB); do [ -x $$b ] || { echo "нет ./$$b — сначала make build"; exit 1; }; done
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
		echo "установлено; демон: sudo systemctl enable --now ramplusd"; \
		echo "веб: sudo ramplus init && sudo systemctl enable --now ramplus-web → http://127.0.0.1:7781"; \
		echo "доступ к демону из CLI: sudo usermod -aG ramplus USER (чтение), -aG ramplus,ramplus-admin (изменения)"; \
	fi

# Журнал /var/lib/ramplus не удаляется: rollback возможен и после переустановки.
# Отказ, если RamPlus ещё управляет swap/профилем (или есть следы без журнала):
# сначала swap remove / profile reset / swap remove --orphan; FORCE=1 — всё равно.
# Группы ramplus, ramplus-admin, пользователь ramplus-web и пароль веба
# (/etc/ramplus/web-auth) удаляются только с PURGE=1.
uninstall:
	@if [ -z "$(DESTDIR)" ] && [ "$(FORCE)" != 1 ]; then \
		rp=$(BINDIR)/$(BINARY); [ -x $$rp ] || rp=./$(BINARY); \
		if [ -x $$rp ] && ! $$rp swap check-clean; then \
			echo; \
			echo "uninstall отменён: RamPlus ещё управляет системой (см. выше)."; \
			echo "Сначала: sudo ramplus swap remove и/или sudo ramplus profile reset"; \
			echo "(следы без записи в журнале: sudo ramplus swap remove --orphan)."; \
			echo "Удалить всё равно, оставив swap/профиль как есть: sudo make uninstall FORCE=1"; \
			exit 1; \
		fi; \
	fi
	@if [ -z "$(DESTDIR)" ]; then \
		for u in ramplus-web ramplusd; do \
			systemctl cat $$u.service >/dev/null 2>&1 || continue; \
			systemctl stop $$u.service; \
			systemctl disable $$u.service; \
			if systemctl is-active --quiet $$u.service; then echo "$$u не остановился"; exit 1; fi; \
			echo "$$u остановлен и отключён"; \
		done; \
	fi
	rm -f $(DESTDIR)$(BINDIR)/$(BINARY) $(DESTDIR)$(BINDIR)/$(DAEMON) $(DESTDIR)$(BINDIR)/$(WEB)
	rm -f $(DESTDIR)$(UNITDIR)/ramplusd.service $(DESTDIR)$(UNITDIR)/ramplus-web.service
	rm -f $(DESTDIR)$(SYSUSERSDIR)/ramplus.conf $(DESTDIR)$(TMPFILESDIR)/ramplus.conf
	@if [ -z "$(DESTDIR)" ]; then \
		systemctl daemon-reload; \
		rm -rf /run/ramplus; \
		if [ "$(PURGE)" = 1 ]; then \
			echo "PURGE=1: удаляются пароль веба, пользователь ramplus-web, группы ramplus и ramplus-admin — их участники теряют членство"; \
			rm -f /etc/ramplus/web-auth; rmdir /etc/ramplus 2>/dev/null || true; \
			if getent passwd ramplus-web >/dev/null; then userdel ramplus-web && echo "пользователь ramplus-web удалён"; fi; \
			for g in ramplus-web ramplus-admin ramplus; do if getent group $$g >/dev/null; then groupdel $$g && echo "группа $$g удалена"; fi; done; \
		else \
			echo "оставлены: пользователь ramplus-web, группы ramplus и ramplus-admin, /etc/ramplus/web-auth (удалить: sudo make uninstall PURGE=1)"; \
		fi; \
	fi
