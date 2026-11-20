// Package i18n — каталог сообщений RamPlus (английский и русский) и выбор
// языка. Ключи — стабильные коды (коды проверок, виды шагов, коды ошибок);
// тексты переводятся при выводе. JSON и API используют те же коды, что и
// раньше, — меняется только язык полей message/description.
package i18n

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Lang — язык вывода.
type Lang string

// Поддерживаемые языки. EN — по умолчанию.
const (
	EN Lang = "en"
	RU Lang = "ru"
)

// Langs — все поддерживаемые языки.
var Langs = []Lang{EN, RU}

// Parse — язык из значения флага, локали (ru_RU.UTF-8) или тега (ru-RU).
func Parse(s string) (Lang, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return EN, false
	case strings.HasPrefix(s, "ru"):
		return RU, true
	case strings.HasPrefix(s, "en"), s == "c", s == "posix", strings.HasPrefix(s, "c."):
		return EN, true
	}
	return EN, false
}

// FromEnv — язык по переменным окружения: первая непустая из LC_ALL,
// LC_MESSAGES, LANG (порядок POSIX); русский — если она начинается с ru.
func FromEnv(getenv func(string) string) Lang {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(k); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "ru") {
				return RU
			}
			return EN
		}
	}
	return EN
}

// FromAcceptLanguage — язык по заголовку Accept-Language (с весами q);
// по умолчанию английский.
func FromAcceptLanguage(h string) Lang {
	type cand struct {
		lang Lang
		q    float64
		pos  int
	}
	var cs []cand
	for i, part := range strings.Split(h, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if _, err := fmt.Sscanf(v, "%g", &q); err != nil {
				q = 0
			}
		}
		if l, ok := Parse(tag); ok && q > 0 {
			cs = append(cs, cand{l, q, i})
		}
	}
	sort.SliceStable(cs, func(a, b int) bool { return cs[a].q > cs[b].q })
	if len(cs) == 0 {
		return EN
	}
	return cs[0].lang
}

// Text — значение, которое переводится при выводе.
type Text interface{ Text(Lang) string }

// M — сообщение каталога: ключ и параметры (как у fmt.Sprintf).
// Параметры-Text и ошибки переводятся на язык вывода.
type M struct {
	Key  string
	Args []any
}

// Msg — сообщение по ключу.
func Msg(key string, args ...any) M { return M{Key: key, Args: args} }

// Text — сообщение на языке l.
func (m M) Text(l Lang) string { return T(l, m.Key, m.Args...) }

// String — английский текст (логи, отпечатки).
func (m M) String() string { return m.Text(EN) }

// Raw — готовая строка, которая не переводится (пути, команды, сообщения ОС).
type Raw string

// Text — сама строка.
func (r Raw) Text(Lang) string { return string(r) }

// Join — переводимые части через разделитель sep.
type Join struct {
	Sep   string
	Parts []Text
}

// Text — части на языке l через Sep.
func (j Join) Text(l Lang) string {
	out := make([]string, len(j.Parts))
	for i, p := range j.Parts {
		out[i] = p.Text(l)
	}
	return strings.Join(out, j.Sep)
}

// T — сообщение key на языке l. Нет такого ключа — сам ключ (тест полноты
// каталога не допускает этого в коде).
func T(l Lang, key string, args ...any) string {
	format, ok := lookup(l, key)
	if !ok {
		format = key
	}
	if len(args) == 0 {
		return strings.ReplaceAll(format, "%%", "%") // литеральный % в тексте без параметров
	}
	return fmt.Sprintf(format, localize(l, args)...)
}

func localize(l Lang, args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case Text:
			out[i] = v.Text(l)
		case error:
			out[i] = ErrText(l, v)
		default:
			out[i] = a
		}
	}
	return out
}

// Plural — форма слова для числа n: в каталоге формы через «|»
// (русский: одна|несколько|много; английский: one|other). Параметр %d — n.
func Plural(l Lang, n int, key string, args ...any) string {
	format, ok := lookup(l, key)
	if !ok {
		format = key
	}
	forms := strings.Split(format, "|")
	f := forms[len(forms)-1]
	switch {
	case (l == RU || l == "" && Default == RU) && len(forms) >= 3:
		m10, m100 := n%10, n%100
		switch {
		case m100 >= 11 && m100 <= 14:
			f = forms[2]
		case m10 == 1:
			f = forms[0]
		case m10 >= 2 && m10 <= 4:
			f = forms[1]
		}
	case n == 1:
		f = forms[0]
	}
	return fmt.Sprintf(f, localize(l, append([]any{n}, args...))...)
}

// Err — ошибка с сообщением каталога. Error() — на языке Default (в
// программе — английский); ErrText — на языке вывода. Ошибки среди параметров доступны через errors.Is/As.
type Err struct{ M }

// Errorf — ошибка по ключу каталога.
func Errorf(key string, args ...any) error { return &Err{Msg(key, args...)} }

func (e *Err) Error() string { return e.Text(Default) }

// Unwrap — ошибки-параметры (для errors.Is/As).
func (e *Err) Unwrap() []error {
	var out []error
	for _, a := range e.Args {
		if err, ok := a.(error); ok {
			out = append(out, err)
		}
	}
	return out
}

// ErrText — текст ошибки на языке l: ошибка каталога (Err) переводится,
// остальные (ошибки ОС и т.п.) — как есть.
func ErrText(l Lang, err error) string {
	if err == nil {
		return ""
	}
	if t, ok := err.(Text); ok {
		return t.Text(l)
	}
	return err.Error()
}

var (
	mu      sync.RWMutex
	catalog = map[string][2]string{} // ключ → {en, ru}
)

// Register добавляет сообщения в каталог (en, ru). Повтор ключа — паника:
// ключи стабильны и уникальны.
func Register(entries map[string][2]string) {
	mu.Lock()
	defer mu.Unlock()
	for k, v := range entries {
		if _, dup := catalog[k]; dup {
			panic("i18n: duplicate key " + k)
		}
		catalog[k] = v
	}
}

// Default — язык для пустого Lang (не задан вызывающим). В программе —
// английский; пакеты тестов с русскими эталонами ставят RU в TestMain.
var Default = EN

func lookup(l Lang, key string) (string, bool) {
	if l == "" {
		l = Default
	}
	mu.RLock()
	defer mu.RUnlock()
	v, ok := catalog[key]
	if !ok {
		return "", false
	}
	if l == RU {
		return v[1], true
	}
	return v[0], true
}

// Has — есть ли ключ в каталоге.
func Has(key string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := catalog[key]
	return ok
}

// Keys — все ключи каталога (для тестов полноты).
func Keys() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(catalog))
	for k := range catalog {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Pair — английский и русский текст ключа.
func Pair(key string) (en, ru string, ok bool) {
	mu.RLock()
	defer mu.RUnlock()
	v, ok := catalog[key]
	return v[0], v[1], ok
}

// Func — значение, вычисляемое на языке вывода (дата, название операции).
// Только для параметров сообщений: в JSON не сериализуется.
type Func func(Lang) string

// Text — значение на языке l.
func (f Func) Text(l Lang) string { return f(l) }
