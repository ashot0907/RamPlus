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
```

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
ramplus swap plan --size 12G                       # план, ничего не меняет, root не нужен
sudo ramplus swap apply --size 12G                 # печатает тот же план и спрашивает подтверждение
sudo ramplus swap apply --size 12G --existing=replace [--delete-old]
sudo ramplus swap resize --size 16G
sudo ramplus swap remove
sudo ramplus rollback                              # отменить последнюю операцию; повторный вызов идёт дальше назад
```

У `apply`, `resize`, `remove` и `rollback` есть `--dry-run` (только план) и
`--yes` (без вопроса, для скриптов).

- **План всегда показывает выбор явно**: размер и допустимый максимум,
  keep или replace для существующего swap, `--delete-old`. Для каждого
  варианта указаны итоговый объём swap и файлы, которые будут созданы,
  отключены или удалены. Если `--existing` не указан, используется
  безопасный `keep`, и план прямо об этом пишет.
- **Проверки до любых изменений.** Если какая-то не пройдена, план
  блокируется:
  - свободное место: после создания swap должно остаться ≥ 10% диска и
    ≥ 5 GiB;
  - ФС: ext4 и xfs — fallocate; btrfs — отдельный NOCOW-подтом
    `/ramplus-swap`; zfs, tmpfs, overlay, nfs — отказ с объяснением;
  - swap на USB-диске работает только с `--force`;
  - при `resume=` в cmdline выводится предупреждение;
  - перед каждым swapoff проверяется, что его данные поместятся в RAM и
    оставшийся swap.
- **Что создаётся:** `/ramplus.swap` (права 600) и systemd-юнит
  `ramplus.swap.swap`. Свой swap RamPlus в `/etc/fstab` не пишет. При
  `replace` строка старого swap в fstab комментируется с пометкой
  `# disabled by ramplus:`, бэкап fstab сохраняется. Старый файл удаляется
  только с `--delete-old`.
- **Журнал** лежит в `/var/lib/ramplus/state.json`: каждый выполненный
  шаг и способ его отменить. Если шаг упал, уже выполненные шаги
  откатываются, и выводится номер шага с ошибкой. `rollback` работает по
  этому журналу.

Интеграционный тест (только в одноразовой VM): `sudo RAMPLUS_VM_TEST=1 scripts/vm-test.sh ./ramplus`.

## Структура

```
cmd/ramplus        CLI
internal/detect    система, диски, ФС
internal/monitor   meminfo, swaps, zswap, zram, PSI, vm sysctl
internal/swap      план, preflight, выполнение с журналом и откатом
scripts/           интеграционный тест для VM
```

Все системные действия в `internal/swap` идут через интерфейсы `FS`,
`Executor` и `Kernel`. Юнит-тесты работают на фейках и ничего не меняют в
системе.

Парсеры `/proc` и `/sys` — это чистые функции (`io.Reader`/`[]byte` →
структура). Они покрыты тестами на fixture-файлах из `testdata/`.

## Лицензия

[Apache-2.0](LICENSE)
