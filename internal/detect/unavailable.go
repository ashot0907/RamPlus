// Package detect определяет железо и окружение: систему, диски, файловые
// системы. Только чтение.
package detect

import (
	"errors"
	"io/fs"
	"syscall"
)

// ErrParse оборачивается парсерами, когда содержимое источника не удалось
// разобрать.
var ErrParse = errors.New("parse error")

// Коды причин, по которым источник недоступен. Стабильная часть API:
// эти же значения будет отдавать веб-API.
const (
	ReasonPermissionDenied = "permission_denied" // нет прав (обычно нужен root)
	ReasonNotPresent       = "not_present"       // файла/модуля нет в системе
	ReasonUnsupported      = "unsupported"       // ядро не поддерживает
	ReasonParseError       = "parse_error"       // неожиданный формат
	ReasonUnavailable      = "unavailable"       // прочие ошибки
)

// Unavailable описывает, почему значение — "n/a".
type Unavailable struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// NewUnavailable классифицирует ошибку чтения источника. Для nil возвращает nil.
func NewUnavailable(err error) *Unavailable {
	if err == nil {
		return nil
	}
	return &Unavailable{Reason: Reason(err), Detail: err.Error()}
}

// Reason переводит ошибку в один из кодов Reason*.
func Reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrPermission):
		return ReasonPermissionDenied
	case errors.Is(err, fs.ErrNotExist):
		return ReasonNotPresent
	case errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSYS):
		// Например, /proc/pressure/* при CONFIG_PSI=y и psi=0.
		return ReasonUnsupported
	case errors.Is(err, ErrParse):
		return ReasonParseError
	default:
		return ReasonUnavailable
	}
}
