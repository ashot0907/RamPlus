# RamPlus

Open-source инструмент для Linux (десктоп и серверы) для управления
виртуальной памятью: swapfile на SSD, zswap, zram, профили, мониторинг
давления на память. Управление через CLI и веб-интерфейс.

> Статус: ранняя разработка. Есть `ramplus status` (только чтение),
> управление swapfile (`ramplus swap plan|apply|resize|remove`), профили,
> `ramplus setup` (swap и профиль одной операцией), `ramplus recommend`,
> `ramplus rollback`, `ramplus history`, демон `ramplusd` с API на unix-сокете
> и веб-интерфейс `ramplus-web`.

## Сборка

Нужны Go (актуальная стабильная версия) и Node.js с npm (только для сборки
фронтенда), только Linux.

```sh
make build     # фронтенд (npm ci && npm run build) + ./ramplus, ./ramplusd, ./ramplus-web
make web       # только фронтенд: web/ → internal/webui/dist
make test      # go test ./...
make web-test  # vitest: словари ru/en, логика интерфейса, окно плана
make lint      # gofmt + go vet + shellcheck
```

Без `make web` Go-часть тоже собирается и тестируется: ramplus-web тогда
отдаёт страницу «фронтенд не собран».

## Использование

```sh
./ramplus status          # таблица
./ramplus status --json   # JSON для скриптов (та же схема, что у будущего веб-API)
./ramplus status --watch [--interval 1s]   # обновление на месте + пики; Ctrl+C — итог
```

PSI интерпретируется по some avg10: < 5 — норма, 5–20 — повышенное
давление, 20–50 — система тормозит, > 50 — сильные тормоза. В JSON это
поле `psi.pressure_level` (`normal|elevated|high|severe`). Пороги заданы
константами в `internal/monitor/psi.go`, это стартовые значения.

Диски виртуальных машин (virtio, xen, Hyper-V, VMware, vd*/xvd*, QEMU)
получают тип `virtual`: их `rotational` недостоверен.

Root не нужен. Источник, который нельзя прочитать, показывается как
`n/a (причина)`, и команда при этом не падает. Например, статистике zswap
из debugfs нужен root.

Что показывает `status`:

| Раздел          | Источник                                                         |
|-----------------|------------------------------------------------------------------|
| Система         | `uname`, `/etc/os-release`                                       |
| Память          | `/proc/meminfo`                                                  |
| Swap-устройства | `/proc/swaps`                                                    |
| zswap           | `/sys/module/zswap/parameters/`, `/proc/meminfo`, debugfs (root) |
| VM              | `/proc/sys/vm/{swappiness,vfs_cache_pressure,watermark_scale_factor}` |
| PSI             | `/proc/pressure/memory`                                          |
| Корневой диск   | `/proc/self/mountinfo`, `/sys/block/*/queue/rotational`, `statfs` |
| zram            | `/sys/block/zram*`                                               |

### JSON-схема

- `schema_version` растёт только при несовместимых изменениях.
- Все размеры указаны в байтах (поля `*_bytes`).
- `null` значит, что значение недоступно. Причина лежит в соседнем поле
  `error` / `*_error`:
  `{"reason": "permission_denied" | "not_present" | "unsupported" | "parse_error" | "unavailable", "detail": "..."}`.
- Списки (`swaps.devices`, `zram.devices`, `root_disk.disks`) всегда
  массивы и никогда не `null`.

## Управление swapfile

```sh
ramplus swap plan --size 12G [--verbose]           # план, ничего не меняет, root не нужен
ramplus swap plan --size 16G --path /mnt/nvme/ramplus.swapfile
sudo ramplus swap apply --size 12G                 # печатает тот же план и спрашивает подтверждение
sudo ramplus swap apply --size 12G --existing=replace [--delete-old]
sudo ramplus swap resize --size 16G
sudo ramplus swap remove
sudo ramplus rollback                              # отменить последнюю операцию; повторный вызов идёт дальше назад
ramplus history [--json]                           # операции из журнала: статус, что отменит следующий rollback
ramplus swap check-clean                           # нет ли следов RamPlus (код 1 — есть)
sudo ramplus swap remove --orphan                  # убрать следы RamPlus без записи в журнале
```

У `apply`, `resize`, `remove` и `rollback` есть `--dry-run` (только план) и
`--yes` (без вопроса, для скриптов).

Для аудита у каждой операции в журнале есть `source` (`cli` — из терминала,
`daemon` — через API демона) и `requested_by` (uid и имя того, кто
запросил). Для CLI это пользователь, вызвавший sudo. Для демона — uid по
SO_PEERCRED, у запросов от пользователя `ramplus-web` стоит пометка
`via: "web"`. `history` показывает это как «из терминала (ashot)» или «через
веб-интерфейс». В журналах, записанных до появления этих полей, источник —
`unknown`.

Следы без записи в журнале (журнал удалён или потерян, а swapfile и юнит
остались) `check-clean` показывает отдельно: «найдены следы RamPlus без
записи в журнале». `swap remove --orphan` убирает их с планом и
подтверждением, как остальные команды, и только после проверки маркеров:
`# Managed by RamPlus` в юните, сигнатура `SWAPSPACE2` в файле, путь RamPlus
по умолчанию или из `What=` юнита с маркером. Если что-то не сходится, план
блокируется проверкой `orphan.not_ours`. Операция записывается в журнал и
откатывается `rollback`.

`rollback` показывает, что именно отменяется. Если операции больше суток —
`[warn] rollback.old` с датой и «N дней назад»; если она другого типа, чем
последняя операция пользователя (например, swap уже отменён, а теперь
откатится профиль), — `[warn] rollback.other_domain`. В JSON у плана есть
`rollback` с `domain` и `age_seconds`.

- **План всегда показывает выбор явно**: размер и допустимый максимум,
  место (`--path`: варианты — смонтированные ФС с пометками USB, сеть,
  виртуальный диск, «не подходит»), keep или replace для существующего
  swap, `--delete-old`. Для каждого
  варианта указаны итоговый объём swap и файлы, которые будут созданы,
  отключены или удалены. Если `--existing` не указан, используется
  безопасный `keep`, и план прямо об этом пишет.
- **Проверки до любых изменений.** Если какая-то не пройдена, план
  блокируется:
  - свободное место на ФС цели: после создания swap должно остаться
    max(5 GiB, min(10% диска, 20 GiB));
  - ФС: ext4 и xfs — fallocate; btrfs — NOCOW-файл (по умолчанию в
    отдельном подтоме `/ramplus-swap`); NTFS, vfat, exfat, fuseblk, zfs,
    tmpfs, overlay, сетевые ФС — отказ с объяснением;
  - виртуальный диск — нейтральное `[info]`, HDD — предупреждение;
  - swap на USB-диске работает только с `--force`;
  - при `resume=` в cmdline выводится предупреждение;
  - перед каждым swapoff проверяется, что его данные поместятся в RAM и
    оставшийся swap.
- **Что создаётся:** `/ramplus.swapfile` (права 600) и systemd-юнит
  `ramplus.swapfile.swap` (`plan --verbose` показывает его целиком). Свой swap RamPlus в `/etc/fstab` не пишет. При
  `replace` строка старого swap в fstab комментируется с пометкой
  `# disabled by ramplus:`, бэкап fstab сохраняется. Старый файл удаляется
  только с `--delete-old`.
- **Журнал** лежит в `/var/lib/ramplus/state.json` и пишется до изменений
  (write-ahead). Перед каждым шагом записывается `pending` и способ
  отмены, после шага — `done`. Если шаг упал, уже выполненные шаги
  откатываются, и выводится номер шага с ошибкой. Если упал сам процесс,
  следующий запуск видит незавершённую операцию, а `rollback` откатывает
  прерванный шаг, только когда его артефакт совпадает с записью (путь,
  размер, права, содержимое).

Интеграционный тест (только в одноразовой VM): `sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus`.

### JSON

`--json` есть у `swap plan/apply/resize/remove`, `rollback`,
`profile plan/apply/reset` и `history` — та же схема, что у будущего API демона:

- план (`plan`, `--dry-run --json`): `schema_version`, `id`, `fingerprint`, `action`, `command`,
  `blocked`, `choices`, `checks` (у каждой проверки стабильный машинный
  `code`, например `fs.unsupported`, `disk.usb`, `rollback.nothing`; тексты —
  в `title`/`message`), `steps` (`kind` и параметры шага плюс `description`),
  `summary`, `swap`/`profile`;
- выполнение (`--json --yes`, подтверждения в JSON-режиме нет): `status`
  (`done`, `nothing`, `blocked`, `cancelled`, `need_root`, `stale`,
  `unknown_plan`, `failed`, `rollback_failed`, `error`), `operation_id`,
  `error`, `events` (шаги и их откат) и `plan`. Код выхода 1, если статус
  не `done`/`nothing`.

`fingerprint` — отпечаток фактов, на которых построен план: активные swap,
ФС и свободное место (с точностью до GiB), fstab, значения параметров
zswap/sysctl, журнал (версия, операции и их статусы), а также решение
плана (шаги и вердикты проверок). Выполнение по ID плана (`core.Apply`,
для демона) заново собирает факты под блокировкой и отказывается со
статусом `stale`, если отпечаток изменился: «система изменилась с момента
построения плана, постройте план заново». CLI строит план и сразу
выполняет его тем же путём.

## Профили

```sh
ramplus profile list                               # профили и статусы
ramplus profile show zswap-lz4                     # параметры и evidence
ramplus profile plan zswap-lz4 [--set vm.swappiness=80] [--verbose]   # без root
sudo ramplus profile apply zswap-lz4 --experimental
ramplus profile current                            # что применено, отличия от исходных значений
sudo ramplus profile reset                         # вернуть значения до первого apply
sudo ramplus rollback                              # общий стек со swap
```

Профиль задаёт параметры zswap (`enabled`, `compressor`, `zpool`,
`max_pool_percent`, `shrinker_enabled`) и sysctl (`vm.swappiness`,
`vm.page-cluster`, `vm.watermark_scale_factor`, `vm.vfs_cache_pressure`).
Параметр, которого нет в профиле, не трогается.

| профиль | статус |
|---|---|
| `default` | стандарт Ubuntu 24.04: zswap выключен, swappiness 60, page-cluster 3, watermark_scale_factor 10, vfs_cache_pressure 100 |
| `zswap-lz4` | экспериментальный: zswap, lz4, zsmalloc, пул 20%, остальное как в default |
| `zswap-zstd` | экспериментальный: то же с zstd |

- **Статусы.** Экспериментальный профиль применяется только с явным
  подтверждением: в терминале — вопрос, в скриптах — `--experimental`.
  «Проверенным» профиль становится только с evidence, то есть с
  результатом `scripts/bench.sh` на реальном SSD; это проверяется при
  загрузке каталога. `--set ключ=значение` меняет параметр поверх
  профиля, и профиль помечается как «изменённый пользователем».
- **Применение.** Значения пишутся сразу в `/sys/module/zswap/parameters`
  и `/proc/sys/vm`. После перезагрузки их восстанавливают
  `/etc/sysctl.d/99-ramplus.conf` и oneshot-юнит `ramplus-zswap.service`
  (значения встроены в юнит). Kernel cmdline и GRUB не меняются.
- **Проверки до изменений:**
  - доступность компрессора (`/proc/crypto` или модуль) и zpool;
  - zswap.* в cmdline — предупреждение;
  - тот же ключ в `/etc/sysctl.d` после нашего файла — предупреждение с
    порядком применения;
  - zswap поверх zram — отказ, обойти можно через `--force`;
  - диапазоны значений `--set`.
- **Журнал.** Исходные значения системы снимаются один раз, при первом
  apply. Перед rollback текущие runtime-значения и содержимое наших
  файлов сверяются с журналом. `ramplus status` показывает активный
  профиль и дрейф, если значения поменяли вручную; в JSON это поля
  `profile` и `drift`.

## Демон ramplusd

HTTP/JSON API поверх unix-сокета `/run/ramplus/ramplusd.sock` (TCP не
слушает). Демон — тонкая оболочка над тем же `internal/core`, что и CLI.
CLI работает напрямую (через sudo); оба можно запускать одновременно:
журнал и блокировка `/run/lock/ramplus.lock` не дают операциям пересечься
(вторая получает отказ, журнал целый).

```sh
make build && sudo make install              # бинарники в /usr/local/bin, юнит, sysusers.d, tmpfiles.d
sudo systemctl enable --now ramplusd
sudo usermod -aG ramplus $USER               # чтение: status, history, plan
sudo usermod -aG ramplus,ramplus-admin $USER # и изменения: apply
ramplus daemon status                        # доступен ли демон, совпадает ли версия с CLI
curl --unix-socket /run/ramplus/ramplusd.sock http://localhost/v1/status
sudo make uninstall                          # журнал /var/lib/ramplus остаётся
```

`make uninstall` сначала проверяет `ramplus swap check-clean`. Если RamPlus
ещё управляет swap или профилем, удаление отменяется с подсказкой: сначала
`swap remove` / `profile reset` (следы без журнала — `swap remove --orphan`);
`FORCE=1` — удалить всё равно. Затем демон останавливается и отключается.
Группы `ramplus` и `ramplus-admin` удаляются только с `PURGE=1`.

Пути, в которые демон не может писать (из-за `ProtectHome=read-only` это
`/home`), блокируют план через демон проверкой
`path.not_writable_by_daemon`: «через веб-интерфейс/демон этот путь
недоступен, используйте CLI». Так операция не падает на шаге создания файла.

Права проверяются по SO_PEERCRED (uid/gid/pid от ядра, дополнительные
группы — из `/proc/<pid>/status`). Сокет `0660 root:ramplus`, каталог
`0750 root:ramplus`: без группы `ramplus` до сокета не добраться. Чтение —
root или `ramplus` (или `ramplus-admin`), изменения — root или
`ramplus-admin`; отказ — `403 auth.forbidden` и запись в журнал демона
(uid, gid, что запрашивалось). `/v1/health` доступен всем, кто дошёл до
сокета.

| эндпоинт | |
|---|---|
| `GET /v1/health` | версия, uptime |
| `GET /v1/status` | то же, что `status --json` |
| `GET /v1/status/stream` | SSE: снимок раз в секунду и пики |
| `GET /v1/history` | то же, что `history --json` |
| `GET /v1/recommendation` | то же, что `recommend --json` (`?path=` — другое место swapfile) |
| `GET /v1/profiles` | каталог профилей, параметры ядра и их текущие значения |
| `POST /v1/plan/setup` | `{swap: {size, existing, delete_old, path} \| null, profile: {name, set} \| null}` |
| `POST /v1/plan/swap` | `{action: apply\|resize\|remove, size: "4G", existing, delete_old, path, force}` |
| `POST /v1/plan/profile` | `{name, set: {"vm.swappiness": "80"}, experimental, force}` |
| `POST /v1/plan/profile-reset`, `POST /v1/plan/rollback` | тело не нужно (`rollback`: `{force}`) |
| `POST /v1/apply` | `{plan_id, confirm_experimental}` → `202 {operation_id}` |
| `GET /v1/operations/{id}` | статус операции, шаги, итог |
| `GET /v1/operations/{id}/events` | SSE: шаги по мере выполнения (`step`), итог (`result`) |

Apply асинхронный. Синхронно проверяется всё, что может отказать без
изменений: план найден и не выполнялся, блокировка свободна, отпечаток не
изменился, проверки пройдены, экспериментальный профиль подтверждён. Только
после записи операции в журнал приходит `202`, а шаги идут в фоне.
Одновременно выполняется одна операция. Планы и операции хранятся в памяти
демона: после перезапуска старый `plan_id` — `plan.not_found`.

Ошибки — `{code, message}`: `auth.forbidden` (403), `plan.not_found` (404),
`plan.used`, `plan.stale`, `op.busy`, `op.locked` (409), `plan.blocked`,
`profile.experimental_not_confirmed` (422), `request.invalid` (400),
`request.too_large` (413, лимит 64 KiB), `op.not_found` (404).

Надёжность: `mlockall` (демон нужен как раз при нехватке памяти и не
должен уходить в swap), `GOMEMLIMIT=64MiB`, `OOMScoreAdjust=-900`. По
SIGTERM демон закрывает потоки, дожидается текущей операции и выходит;
логи (key=value) идут в journald. Выбор защит systemd с объяснением — в
комментариях [packaging/systemd/ramplusd.service](packaging/systemd/ramplusd.service).

## Рекомендация и setup

`ramplus recommend [--json] [--path P]` — что RamPlus советует для этой
машины (только чтение):

- размер swap: RAM ≤ 8 GB → как RAM, 8–32 GB → половина, больше → 16 GB.
  RAM берётся из MemTotal с округлением вверх до целого GB (ядро
  резервирует часть установленной памяти). Затем размер ограничивается
  местом на ФС (запас свободного места — как в плане) и округляется вниз.
  Меньше 1 GB — swap не рекомендуется, с причиной;
- `existing`: `replace`, если существующий swap — файл и его данные
  помещаются в RAM и новый swap; `keep` — раздел, zram или небезопасный
  swapoff; `null` — другого swap нет;
- профиль: `zswap-lz4` с его реальным статусом (`experimental` —
  интерфейс показывает «предварительно»); нет zswap в ядре или есть zram —
  `default`;
- рекомендация проверяется тем же планировщиком: если план заблокирован
  (система на USB-диске, нет места), рекомендация говорит об этом
  (`swap_size_bytes: null`, `swap_reason`, `swap_blocked`).

`ramplus setup` — swap и профиль одной операцией: проверки обеих частей,
шаги подряд (сначала swap, потом профиль), один отпечаток, одна запись в
журнале, один `rollback`. Любая часть может отсутствовать. Отказ любой части
блокирует весь план. Swap задаётся как желаемое состояние: своего swap нет —
создаётся, размер другой — меняется, такой же — без изменений.

```sh
ramplus setup --size 16G --existing=replace --profile zswap-lz4 --dry-run
sudo ramplus setup --size 16G --existing=replace --profile zswap-lz4 --experimental
ramplus history     # «Запас 16 GB из SSD + режим «Сжатие — быстрое»»
sudo ramplus rollback
```

## Веб-интерфейс

`ramplus-web` отдаёт встроенный фронтенд (React + TypeScript, `web/`) и
проксирует `/api/*` на сокет демона `/v1/*` по явному списку путей. Работает
не от root: системный пользователь `ramplus-web` в группах `ramplus` и
`ramplus-admin`. На диск ничего не пишет.

```sh
make build && sudo make install
sudo ramplus init                     # пароль веба (или --password-stdin для скриптов)
sudo systemctl enable --now ramplusd ramplus-web
xdg-open http://127.0.0.1:7781
```

Сервер без графики — через SSH-туннель (веб слушает только 127.0.0.1):

```sh
ssh -L 7781:localhost:7781 user@server   # затем на своём компьютере: http://127.0.0.1:7781
```

Слушать другой адрес можно только с TLS, иначе ramplus-web не стартует:
`--listen 0.0.0.0:7781 --tls-self-signed --host server.lan` (отпечаток
сертификата — в журнале) или `--tls-cert`/`--tls-key`. Параметры добавляются
в `ExecStart` через `systemctl edit ramplus-web`.

Безопасность:

- пароль — хеш argon2id (64 MiB, t=3) в `/etc/ramplus/web-auth`
  (`root:ramplus-web`, 0640); сменить — `sudo ramplus passwd` (открытые
  сессии завершаются), отключить вход — `sudo ramplus passwd --disable`;
- сессия в cookie: HttpOnly, SameSite=Strict, Secure и префикс `__Host-` при
  TLS, срок 12 ч; выход удаляет сессию. Сессии хранятся в памяти
  ramplus-web: после перезапуска нужно войти снова;
- CSRF-токен (`X-CSRF-Token`) на всех изменяющих запросах, плюс проверка
  Origin и Sec-Fetch-Site;
- 5 неудачных входов за 5 минут с адреса — пауза 30 с, каждая следующая
  вдвое длиннее (до 15 мин). Неудачные входы попадают в журнал systemd, пароли — нет;
- проверка Host (защита от DNS rebinding): разрешены `127.0.0.1`,
  `localhost`, `[::1]` и имена из `--host`, иначе 421;
- строгий CSP без inline-скриптов и внешних источников, `X-Frame-Options:
  DENY`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`;
  шрифты Inter и Manrope (с кириллицей) лежат в бандле;
- пароль не задан — веб показывает только «выполните sudo ramplus init».

Интерфейс на русском и английском (по умолчанию — язык браузера). Коды
проверок плана переводятся по коду; если перевода нет, показывается текст с
сервера. Выбор защит systemd с объяснением — в
[packaging/systemd/ramplus-web.service](packaging/systemd/ramplus-web.service).
`make uninstall PURGE=1` удаляет и пользователя `ramplus-web`, и файл пароля.

## Бенчмарк

`scripts/bench.sh` сравнивает no-swap, swap и три конфигурации zswap на
горячей и холодной нагрузке, со сжимаемыми и случайными данными: PSI,
пик swap, срабатывания лимита пула zswap, latency отзывчивости, медиана
из 3 прогонов, автоматический раздел «Наблюдения». Подробности и
предупреждения — в [scripts/BENCH.md](scripts/BENCH.md). Цифры из VM для
выбора значений по умолчанию не используются.

## Структура

```
cmd/ramplus        CLI — тонкая оболочка над internal/core
cmd/ramplusd       демон — тонкая оболочка над internal/core (через internal/api)
cmd/ramplus-web    веб-сервер (не root) — тонкая оболочка над internal/web
cmd/ramplus-probe  нагрузка, latency-probe, сэмплер и отчёт для bench.sh
internal/core      сервисный слой: статус, планы, выполнение, операции демона
internal/api       API демона: unix-сокет, SO_PEERCRED, хендлеры, SSE, клиент
internal/web       ramplus-web: вход, сессии, CSRF, проверка Host, прокси /api → демон
internal/webauth   пароль веба: argon2id, формат /etc/ramplus/web-auth
internal/webui     собранный фронтенд (go:embed)
web/               фронтенд: React + TypeScript (Vite), vitest
internal/plan      модель плана — данные (JSON): проверки, выбор, шаги, итог
internal/journal   журнал state.json, write-ahead, стек отката, сверка состояния
internal/swap      swap: факты, планировщик, шаги
internal/profile   профили: каталог, параметры, sysctl.d, планировщик, шаги
internal/render    текстовый вывод для CLI
internal/sys       интерфейсы FS/Executor/Kernel и реальные реализации
internal/fakesys   фейковая система для тестов
internal/detect    система, диски, ФС
internal/monitor   meminfo, swaps, zswap, zram, PSI, vm sysctl
packaging/         systemd-юниты ramplusd и ramplus-web, sysusers.d, tmpfiles.d
scripts/           интеграционный тест для VM, бенчмарк
```

Все системные действия идут через интерфейсы `FS`, `Executor` и `Kernel`
(`internal/sys`). Юнит-тесты работают на фейке (`internal/fakesys`) и ничего
не меняют в системе. Golden-тесты (`cmd/ramplus/testdata/golden`) фиксируют
текстовый вывод CLI.

Парсеры `/proc` и `/sys` — это чистые функции (`io.Reader`/`[]byte` →
структура). Они покрыты тестами на fixture-файлах из `testdata/`.

## Лицензия

[Apache-2.0](LICENSE)
