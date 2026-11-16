# RamPlus

Open-source инструмент для Linux (десктоп и серверы): управление
виртуальной памятью — swapfile на SSD, zswap, профили, мониторинг
давления на память, позже проактивное вытеснение фоновых
приложений/сервисов на SSD. Управление через CLI и веб-интерфейс,
в том числе удалённо.

## Компоненты
- ramplus — CLI
- ramplusd — root-демон, вся логика, API через unix socket (позже)
- ramplus-web — HTTP-сервер без root, отдаёт фронтенд и
  проксирует запросы к демону (позже)

## Стек
- Go (актуальная стабильная версия), Linux only
- Зависимости минимальные: stdlib, golang.org/x/sys, cobra для CLI
- Фронтенд: React + TypeScript (Vite), встраивается через go:embed
- Python только в scripts/ для тестов и графиков

## Структура
- cmd/ramplus, cmd/ramplusd, cmd/ramplus-web
- internal/detect — железо, диски, FS
- internal/monitor — /proc/meminfo, /proc/swaps, PSI, zswap
- internal/swap, internal/zswap, internal/profile — изменение системы
- internal/api — API демона
- web/ — React-фронтенд
- packaging/ — systemd, polkit
- scripts/ — нагрузочные тесты

## Безопасность
- Пароль задаётся только через `sudo ramplus init`, хранится как
  argon2id-хеш в /etc/ramplus/auth (права 600)
- Сессии в HttpOnly + SameSite=Strict cookie, CSRF, rate limit на вход
- Веб по умолчанию слушает только 127.0.0.1; удалённый доступ
  через SSH-туннель или явно включённый режим --listen с обязательным TLS
- HTTP-сервер никогда не работает от root

## Правила
- Любой код, который меняет систему, должен иметь dry-run и откат
- Парсеры /proc и /sys — чистые функции, покрыты тестами на
  fixture-файлах в testdata/
- Не падать, если файл недоступен (нет прав, старое ядро): "n/a"
- Никаких изменений системы без явного указания в задаче
