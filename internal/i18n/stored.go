package i18n

import (
	"encoding/json"
	"fmt"
)

// Stored — сообщение в виде данных (для журнала): ключ каталога и
// параметры, без привязки к языку. Plain — готовый текст, если это не
// сообщение каталога (ошибка ОС, путь).
type Stored struct {
	Key   string      `json:"key,omitempty"`
	Args  []StoredArg `json:"args,omitempty"`
	Plain string      `json:"text,omitempty"`
	Sep   *string     `json:"sep,omitempty"` // Join: части — Args
}

// StoredArg — параметр: строка, число или вложенное сообщение.
type StoredArg struct {
	S   *string         `json:"s,omitempty"`
	N   json.Number     `json:"n,omitempty"`
	Msg *Stored         `json:"msg,omitempty"`
	Raw json.RawMessage `json:"-"`
}

// Messager — ошибка (или значение), которое можно сохранить как
// сообщение каталога.
type Messager interface{ Message() Text }

// Store — значение (сообщение, ошибка) в виде данных для журнала.
func Store(v any) *Stored {
	switch t := v.(type) {
	case nil:
		return nil
	case Messager:
		return Store(t.Message())
	case *Err:
		return Store(t.M)
	case M:
		s := &Stored{Key: t.Key}
		for _, a := range t.Args {
			s.Args = append(s.Args, storeArg(a))
		}
		return s
	case Join:
		sep := t.Sep
		s := &Stored{Sep: &sep}
		for _, p := range t.Parts {
			s.Args = append(s.Args, storeArg(p))
		}
		return s
	case Raw:
		return &Stored{Plain: string(t)}
	case Text:
		return &Stored{Plain: t.Text(EN)}
	case error:
		return &Stored{Plain: t.Error()}
	}
	return &Stored{Plain: fmt.Sprint(v)}
}

func storeArg(a any) StoredArg {
	switch v := a.(type) {
	case string:
		return StoredArg{S: &v}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return StoredArg{N: json.Number(fmt.Sprint(v))}
	case float32, float64:
		return StoredArg{N: json.Number(fmt.Sprint(v))}
	case Raw:
		s := string(v)
		return StoredArg{S: &s}
	case Messager, *Err, M, Join, Text, error:
		return StoredArg{Msg: Store(v)}
	}
	s := fmt.Sprint(a)
	return StoredArg{S: &s}
}

// Text — сообщение на языке l.
func (s *Stored) Text(l Lang) string {
	if s == nil {
		return ""
	}
	args := make([]any, len(s.Args))
	for i, a := range s.Args {
		args[i] = a.value()
	}
	switch {
	case s.Sep != nil:
		parts := make([]Text, len(args))
		for i, a := range args {
			parts[i] = asText(a)
		}
		return Join{Sep: *s.Sep, Parts: parts}.Text(l)
	case s.Key != "":
		return T(l, s.Key, args...)
	}
	return s.Plain
}

func (a StoredArg) value() any {
	switch {
	case a.Msg != nil:
		return a.Msg
	case a.S != nil:
		return *a.S
	case a.N != "":
		if n, err := a.N.Int64(); err == nil {
			return n
		}
		f, _ := a.N.Float64()
		return f
	}
	return ""
}

func asText(v any) Text {
	if t, ok := v.(Text); ok {
		return t
	}
	return Raw(fmt.Sprint(v))
}
