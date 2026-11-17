# RamPlus

Open-source инструмент для Linux (десктоп и серверы) для управления
виртуальной памятью: swapfile на SSD, zswap, zram, профили, мониторинг
давления на память. Управление через CLI, позже — через веб-интерфейс.

> Статус: ранняя разработка. Есть `ramplus status` (только чтение) и
> управление swapfile: `ramplus swap plan|apply|resize|remove`, `ramplus rollback`.

## Сборка

Нужен Go (актуальная стабильная версия), только Linux.

```sh
make build   # собирает ./ramplus
make test    # go test ./...
make lint    # gofmt + go vet
```

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
ramplus swap check-clean                           # нет ли следов RamPlus (код 1 — есть)
```

У `apply`, `resize`, `remove` и `rollback` есть `--dry-run` (только план) и
`--yes` (без вопроса, для скриптов).

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

## Бенчмарк

`scripts/bench.sh` сравнивает no-swap, swap и три конфигурации zswap на
горячей и холодной нагрузке, со сжимаемыми и случайными данными: PSI,
пик swap, срабатывания лимита пула zswap, latency отзывчивости, медиана
из 3 прогонов, автоматический раздел «Наблюдения». Подробности и
предупреждения — в [scripts/BENCH.md](scripts/BENCH.md). Цифры из VM для
выбора значений по умолчанию не используются.

## Структура

```
cmd/ramplus        CLI
internal/detect    система, диски, ФС
internal/monitor   meminfo, swaps, zswap, zram, PSI, vm sysctl
internal/swap      план, preflight, выполнение с журналом и откатом
cmd/ramplus-probe  нагрузка, latency-probe, сэмплер и отчёт для bench.sh
scripts/           интеграционный тест для VM, бенчмарк
```

Все системные действия в `internal/swap` идут через интерфейсы `FS`,
`Executor` и `Kernel`. Юнит-тесты работают на фейках и ничего не меняют в
системе.

Парсеры `/proc` и `/sys` — это чистые функции (`io.Reader`/`[]byte` →
структура). Они покрыты тестами на fixture-файлах из `testdata/`.

## Лицензия

[Apache-2.0](LICENSE)
