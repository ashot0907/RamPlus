package i18n

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var verbRe = regexp.MustCompile(`%(\[(\d+)\])?[-+# 0]*\d*(\.\d+)?([a-zA-Z%])`)

// verbs — глагол fmt для каждого аргумента (с учётом %[n]s).
func verbs(format string) map[int]string {
	out := map[int]string{}
	next := 1
	format = strings.ReplaceAll(format, "%%", "")
	for _, m := range verbRe.FindAllStringSubmatch(format, -1) {
		if m[4] == "%" {
			continue
		}
		n := next
		if m[2] != "" {
			n = int(m[2][0] - '0')
			if len(m[2]) > 1 {
				n = 0
				for _, c := range m[2] {
					n = n*10 + int(c-'0')
				}
			}
		}
		out[n] = m[4]
		next = n + 1
	}
	return out
}

// У каждого ключа есть английский и русский текст с теми же параметрами.
func TestCatalogComplete(t *testing.T) {
	keys := Keys()
	if len(keys) < 700 {
		t.Fatalf("catalog has only %d keys", len(keys))
	}
	for _, k := range keys {
		en, ru, _ := Pair(k)
		if strings.TrimSpace(en) == "" || strings.TrimSpace(ru) == "" {
			t.Errorf("%s: empty en=%q ru=%q", k, en, ru)
			continue
		}
		enForms, ruForms := strings.Split(en, "|"), strings.Split(ru, "|")
		want := verbs(ruForms[0])
		for _, f := range append(enForms, ruForms...) {
			if got := verbs(f); !equalVerbs(got, want) {
				t.Errorf("%s: parameters differ: %q vs %q", k, f, ruForms[0])
			}
		}
	}
}

func equalVerbs(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w := b[k]
		if v != w && !(isStr(v) && isStr(w)) {
			return false
		}
	}
	return true
}

func isStr(v string) bool { return v == "s" || v == "v" || v == "q" }

// Каждый ключ, который код передаёт в каталог литералом, в каталоге есть.
func TestKeysUsedInCode(t *testing.T) {
	root := filepath.Join("..", "..")
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`i18n\.(?:Msg|Errorf)\("([^"]+)"`),
		regexp.MustCompile(`i18n\.T\([^,()]+, "([^"]+)"`),
		regexp.MustCompile(`i18n\.Plural\([^,]+, [^,]+, "([^"]+)"`),
		regexp.MustCompile(`(?:^|[^\w.])T\("([^"]+)"`),
		regexp.MustCompile(`\bt\(l, "([^"]+)"`),
		regexp.MustCompile(`p\.T\("([^"]+)"`),
	}
	checkAdd := regexp.MustCompile(`(?:p\.add|p\.Add|p\.Plan\.Add)\((?:plan\.)?Level\w+, "([^"]+)"`)
	addKey := regexp.MustCompile(`AddKey\((?:plan\.)?Level\w+, "([^"]+)", "([^"]+)"`)
	var missing []string
	need := func(file, key string) {
		if strings.HasSuffix(key, ".") { // "profile."+имя — ключ собирается в коде
			return
		}
		if !Has(key) {
			missing = append(missing, file+": "+key)
		}
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "web" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "internal/i18n/") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				need(path, m[1])
			}
		}
		for _, m := range checkAdd.FindAllStringSubmatch(src, -1) {
			need(path, "check."+m[1])
			need(path, "check."+m[1]+".title")
		}
		for _, m := range addKey.FindAllStringSubmatch(src, -1) {
			need(path, "check."+m[1]+".title")
			need(path, "check."+m[2])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("not in catalog: %s", m)
	}
}

func TestLanguageSelection(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want Lang
	}{
		{map[string]string{}, EN},
		{map[string]string{"LANG": "ru_RU.UTF-8"}, RU},
		{map[string]string{"LANG": "de_DE.UTF-8"}, EN},
		{map[string]string{"LANG": "ru_RU.UTF-8", "LC_MESSAGES": "en_US.UTF-8"}, EN},
		{map[string]string{"LANG": "en_US.UTF-8", "LC_ALL": "ru_RU.UTF-8"}, RU},
		{map[string]string{"LC_MESSAGES": "ru_RU"}, RU},
		{map[string]string{"LANG": "C.UTF-8"}, EN},
	}
	for _, c := range cases {
		if got := FromEnv(env(c.env)); got != c.want {
			t.Errorf("FromEnv(%v) = %s, want %s", c.env, got, c.want)
		}
	}
	for h, want := range map[string]Lang{
		"": EN, "ru": RU, "ru-RU,ru;q=0.9,en;q=0.8": RU, "en-US,en;q=0.9,ru;q=0.8": EN,
		"de-DE,ru;q=0.5": RU, "fr, de": EN, "en;q=0.1, ru;q=0.9": RU, "ru;q=0": EN,
	} {
		if got := FromAcceptLanguage(h); got != want {
			t.Errorf("FromAcceptLanguage(%q) = %s, want %s", h, got, want)
		}
	}
}

func TestPlural(t *testing.T) {
	want := map[int]string{1: "1 день назад", 2: "2 дня назад", 5: "5 дней назад", 11: "11 дней назад", 21: "21 день назад", 22: "22 дня назад"}
	for n, w := range want {
		if got := Plural(RU, n, "age.days"); got != w {
			t.Errorf("RU %d: %q", n, got)
		}
	}
	if Plural(EN, 1, "age.days") != "1 day ago" || Plural(EN, 3, "age.days") != "3 days ago" {
		t.Error("EN plural")
	}
}

// Ошибка сохраняется в журнал как данные и переводится при выводе.
func TestStoredRoundTrip(t *testing.T) {
	inner := Errorf("journal.write_failed", "/var/lib/ramplus/state.json", errors.New("disk full"))
	err := Errorf("journal.update_failed_after_error", Errorf("journal.not_ours"), inner)
	b, jerr := json.Marshal(Store(err))
	if jerr != nil {
		t.Fatal(jerr)
	}
	var back Stored
	if jerr := json.Unmarshal(b, &back); jerr != nil {
		t.Fatal(jerr)
	}
	for _, l := range Langs {
		if got, want := back.Text(l), ErrText(l, err); got != want {
			t.Errorf("%s: %q, want %q", l, got, want)
		}
	}
	if !strings.Contains(back.Text(RU), "запись журнала") || !strings.Contains(back.Text(EN), "writing the journal") {
		t.Errorf("not localized: %s / %s", back.Text(EN), back.Text(RU))
	}
	if !errors.Is(err, inner) {
		t.Error("errors.Is through i18n.Err")
	}
	n := Store(Msg("check.rollback.target", "op1", "2026-11-17 10:00:00", "ramplus rollback"))
	b, _ = json.Marshal(n)
	var m Stored
	_ = json.Unmarshal(b, &m)
	if m.Text(EN) != "undoing operation op1 from 2026-11-17 10:00:00: ramplus rollback" {
		t.Errorf("got %q", m.Text(EN))
	}
}
