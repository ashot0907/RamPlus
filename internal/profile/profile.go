// Package profile — профили RamPlus: каталог встроенных профилей, реестр
// настраиваемых параметров (zswap, sysctl vm.*), разбор sysctl.d и
// содержимое файлов, которые делают профиль постоянным. Только чистые
// функции; применение — internal/swap (общий журнал и стек отката).
package profile

import (
	"embed"

	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ashot0907/ramplus/internal/i18n"
)

// Статусы профиля.
const (
	StatusStandard     = "standard"     // значения стоковой системы
	StatusExperimental = "experimental" // не подтверждён бенчмарком на SSD
	StatusVerified     = "verified"     // подтверждён scripts/bench.sh на реальном SSD (нужен evidence)
	StatusModified     = "modified"     // профиль + --set пользователя
)

// StatusText — статус для человека.
func StatusText(l i18n.Lang, status string) string {
	switch status {
	case StatusStandard, StatusExperimental, StatusVerified, StatusModified:
		return i18n.T(l, "profile.status."+status)
	}
	return status
}

// Profile — встроенный профиль.
type Profile struct {
	// Name — имя профиля. Название, описание и название режима для человека
	// — в каталоге сообщений: profile.<name>.title / .description / .friendly.
	Name   string `json:"name"`
	Status string `json:"status"`
	// Evidence — ссылка на результат scripts/bench.sh на реальном SSD.
	Evidence string `json:"evidence"`
	// Comment — откуда значения (правило CLAUDE.md: ссылка на результат).
	Comment string `json:"comment"`
	// Params — ключ → значение; отсутствующий ключ = «не трогать».
	Params map[string]string `json:"params"`
}

//go:embed profiles/*.json
var profileFS embed.FS

var catalog = mustLoad()

func mustLoad() map[string]Profile {
	m, err := load()
	if err != nil {
		panic(err) // встроенные профили проверяются тестами
	}
	return m
}

func load() (map[string]Profile, error) {
	entries, err := profileFS.ReadDir("profiles")
	if err != nil {
		return nil, err
	}
	m := map[string]Profile{}
	for _, e := range entries {
		b, err := profileFS.ReadFile("profiles/" + e.Name())
		if err != nil {
			return nil, err
		}
		var p Profile
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if want := strings.TrimSuffix(e.Name(), path.Ext(e.Name())); p.Name != want {
			return nil, i18n.Errorf("profile.err.name_mismatch", e.Name(), p.Name)
		}
		for _, k := range []string{".title", ".description", ".friendly"} {
			if !i18n.Has("profile." + p.Name + k) {
				return nil, fmt.Errorf("%s: no catalog entry profile.%s%s", e.Name(), p.Name, k)
			}
		}
		if err := Validate(p); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		m[p.Name] = p
	}
	return m, nil
}

// Validate проверяет профиль: известные ключи, допустимые значения,
// статус "проверенный" — только с evidence.
func Validate(p Profile) error {
	switch p.Status {
	case StatusStandard, StatusExperimental:
	case StatusVerified:
		if strings.TrimSpace(p.Evidence) == "" {
			return i18n.Errorf("profile.err.verified_without_evidence", p.Status)
		}
	default:
		return i18n.Errorf("profile.err.unknown_status", p.Status)
	}
	if len(p.Params) == 0 {
		return i18n.Errorf("profile.err.no_params", p.Name)
	}
	for k, v := range p.Params {
		n, err := Normalize(k, v)
		if err != nil {
			return err
		}
		if n != v {
			return i18n.Errorf("profile.err.not_normalized", k, v, n)
		}
	}
	return nil
}

// All — профили в порядке: default, затем по имени.
func All() []Profile {
	out := make([]Profile, 0, len(catalog))
	for _, p := range catalog {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name == "default") != (out[j].Name == "default") {
			return out[i].Name == "default"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// FriendlyName — название режима для человека («Сжатие — быстрое»);
// неизвестный профиль — его имя.
func FriendlyName(l i18n.Lang, name string) string {
	if i18n.Has("profile." + name + ".friendly") {
		return i18n.T(l, "profile."+name+".friendly")
	}
	return name
}

// Title — название профиля («zswap + lz4»).
func (p Profile) Title(l i18n.Lang) string { return i18n.T(l, "profile."+p.Name+".title") }

// Description — описание профиля.
func (p Profile) Description(l i18n.Lang) string { return i18n.T(l, "profile."+p.Name+".description") }

// Friendly — название режима для человека.
func (p Profile) Friendly(l i18n.Lang) string { return FriendlyName(l, p.Name) }

// ParamTitle — что это за параметр, для человека.
func ParamTitle(l i18n.Lang, key string) string { return i18n.T(l, "param."+key) }

// Get — профиль по имени.
func Get(name string) (Profile, bool) {
	p, ok := catalog[name]
	return p, ok
}

// Names — имена профилей.
func Names() []string {
	var out []string
	for _, p := range All() {
		out = append(out, p.Name)
	}
	return out
}

// Kind — тип значения параметра.
type Kind int

const (
	KindBool Kind = iota // Y/N
	KindInt
	KindWord
)

// Group — где живёт параметр.
const (
	GroupZswap  = "zswap"
	GroupSysctl = "sysctl"
)

// Param — настраиваемый параметр.
type Param struct {
	Key      string
	Path     string // runtime-файл
	Group    string
	Kind     Kind
	Min, Max int
	Words    []string // допустимые слова (пусто — любое [a-z0-9-])
}

// ZswapParamDir — параметры модуля zswap.
const ZswapParamDir = "/sys/module/zswap/parameters"

// Params — все параметры, которые может менять профиль (порядок вывода).
var Params = []Param{
	{Key: "zswap.enabled", Path: ZswapParamDir + "/enabled", Group: GroupZswap, Kind: KindBool},
	{Key: "zswap.compressor", Path: ZswapParamDir + "/compressor", Group: GroupZswap, Kind: KindWord},
	{Key: "zswap.zpool", Path: ZswapParamDir + "/zpool", Group: GroupZswap, Kind: KindWord, Words: []string{"zsmalloc", "zbud", "z3fold"}},
	{Key: "zswap.max_pool_percent", Path: ZswapParamDir + "/max_pool_percent", Group: GroupZswap, Kind: KindInt, Min: 1, Max: 100},
	{Key: "zswap.shrinker_enabled", Path: ZswapParamDir + "/shrinker_enabled", Group: GroupZswap, Kind: KindBool},
	{Key: "vm.swappiness", Path: "/proc/sys/vm/swappiness", Group: GroupSysctl, Kind: KindInt, Min: 0, Max: 200},
	{Key: "vm.page-cluster", Path: "/proc/sys/vm/page-cluster", Group: GroupSysctl, Kind: KindInt, Min: 0, Max: 10},
	{Key: "vm.watermark_scale_factor", Path: "/proc/sys/vm/watermark_scale_factor", Group: GroupSysctl, Kind: KindInt, Min: 1, Max: 3000},
	{Key: "vm.vfs_cache_pressure", Path: "/proc/sys/vm/vfs_cache_pressure", Group: GroupSysctl, Kind: KindInt, Min: 0, Max: 10000},
}

// Lookup — параметр по ключу.
func Lookup(key string) (Param, bool) {
	for _, p := range Params {
		if p.Key == key {
			return p, true
		}
	}
	return Param{}, false
}

// KeyForPath — ключ параметра по runtime-пути ("" — не параметр профиля).
func KeyForPath(p string) string {
	for _, pr := range Params {
		if pr.Path == p {
			return pr.Key
		}
	}
	return ""
}

var wordRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Normalize проверяет значение и приводит к виду, который пишется в ядро
// и который ядро возвращает при чтении (bool → Y/N).
func Normalize(key, value string) (string, error) {
	p, ok := Lookup(key)
	if !ok {
		return "", i18n.Errorf("param.err.unknown", key, strings.Join(Keys(), ", "))
	}
	v := strings.TrimSpace(value)
	switch p.Kind {
	case KindBool:
		switch strings.ToLower(v) {
		case "y", "1", "yes", "on", "true":
			return "Y", nil
		case "n", "0", "no", "off", "false":
			return "N", nil
		}
		return "", i18n.Errorf("param.err.bool", key, value)
	case KindInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", i18n.Errorf("param.err.int", key, value)
		}
		if n < p.Min || n > p.Max {
			return "", i18n.Errorf("param.err.range", key, n, p.Min, p.Max)
		}
		return strconv.Itoa(n), nil
	default:
		if !wordRe.MatchString(v) {
			return "", i18n.Errorf("param.err.word", key, value)
		}
		if len(p.Words) > 0 && !contains(p.Words, v) {
			return "", i18n.Errorf("param.err.words", key, v, strings.Join(p.Words, ", "))
		}
		return v, nil
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Keys — ключи всех параметров.
func Keys() []string {
	out := make([]string, len(Params))
	for i, p := range Params {
		out[i] = p.Key
	}
	return out
}

// ParseSet разбирает --set ключ=значение (с проверкой значений).
func ParseSet(items []string) (map[string]string, error) {
	out := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok {
			return nil, i18n.Errorf("param.err.set_syntax", it)
		}
		n, err := Normalize(strings.TrimSpace(k), v)
		if err != nil {
			return nil, i18n.Errorf("cli.flag_error", "--set", err)
		}
		out[strings.TrimSpace(k)] = n
	}
	return out, nil
}

// Effective — параметры профиля с наложенными --set.
func Effective(p Profile, overrides map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range p.Params {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ZswapWriteOrder — порядок записи параметров zswap: при включении
// enabled пишется последним (сначала компрессор и пул), при выключении —
// первым.
func ZswapWriteOrder(enabling bool) []string {
	keys := []string{"zswap.compressor", "zswap.zpool", "zswap.max_pool_percent", "zswap.shrinker_enabled"}
	if enabling {
		return append(keys, "zswap.enabled")
	}
	return append([]string{"zswap.enabled"}, keys...)
}
