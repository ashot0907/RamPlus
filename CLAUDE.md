# RamPlus

Open-source инструмент для Linux (десктоп и серверы): управление
виртуальной памятью — swapfile на SSD, zswap, профили, мониторинг
давления на память, позже проактивное вытеснение фоновых
приложений/сервисов на SSD. Управление через CLI и веб-интерфейс,
в том числе удалённо.

## Компоненты
- ramplus — CLI
- ramplusd — root-демон: HTTP/JSON API через unix socket, тонкая оболочка над internal/core
- ramplus-web — HTTP-сервер без root (пользователь ramplus-web), отдаёт
  встроенный фронтенд и проксирует /api/* к демону по списку путей

## Стек
- Go (актуальная стабильная версия), Linux only
- Зависимости минимальные: stdlib, golang.org/x/sys, cobra для CLI,
  golang.org/x/crypto только ради argon2id
- Фронтенд: React + TypeScript (Vite), без UI- и CSS-фреймворков,
  встраивается через go:embed (web/ → internal/webui/dist)
- Python только в scripts/ для тестов и графиков

## Структура
- cmd/ramplus, cmd/ramplusd, cmd/ramplus-web — тонкие оболочки над internal/core
- internal/core — сервисный слой: Status, планы (swap, профили, setup,
  rollback), рекомендация, выполнение, история, пароль веба; единая точка
  входа для CLI, демона и веба
- internal/plan — модель плана (данные, JSON): проверки, выбор, шаги, итог
- internal/journal — движок изменений: state.json, операции, шаги и их
  отмена, write-ahead, стек отката, сверка состояния, миграции, блокировка;
  про swap и профили не знает (шаги регистрируются через journal.Register)
- internal/swap — swap: факты, планировщик, шаги
- internal/profile — профили: каталог, планировщик, шаги
- internal/render — текстовый вывод для CLI (план, ход выполнения, статус)
- internal/sys — интерфейсы FS/Executor/Kernel и реальные реализации
- internal/fakesys — фейковая система для тестов
- internal/detect — железо, диски, FS
- internal/monitor — /proc/meminfo, /proc/swaps, PSI, zswap
- internal/api — API демона: unix socket, SO_PEERCRED, хендлеры, SSE, клиент
- internal/web — ramplus-web: вход, сессии, CSRF, rate limit, проверка Host,
  заголовки безопасности, прокси /api → демон, TLS
- internal/webauth — argon2id, формат файла пароля (чистые функции)
- internal/webui — собранный фронтенд (go:embed)
- web/ — React-фронтенд (словари ru/en, vitest)
- packaging/ — systemd-юниты ramplusd и ramplus-web, sysusers.d, tmpfiles.d
  (позже polkit)
- scripts/ — нагрузочные тесты

## Безопасность
- Пароль задаётся только через `sudo ramplus init` (смена — `ramplus
  passwd`), хранится как argon2id-хеш в /etc/ramplus/web-auth
  (root:ramplus-web, 0640)
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
- Логика только в internal/core и ниже; CLI, демон и веб — тонкие
  оболочки над internal/core
- Любое значение по умолчанию в профилях (zswap, swappiness и т.д.)
  должно быть подтверждено результатом scripts/bench.sh на реальном SSD;
  ссылка на результат — в комментарии к профилю
- Цифры производительности из VM не используются для выбора значений
  по умолчанию
