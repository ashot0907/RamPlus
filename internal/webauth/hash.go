// Package webauth — пароль веб-интерфейса: хеш argon2id, формат файла
// /etc/ramplus/web-auth, проверка пароля. Чистые функции; запись файла —
// internal/core, вход и сессии — internal/web.
package webauth

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Файл пароля.
const (
	DefaultPath = "/etc/ramplus/web-auth"
	Dir         = "/etc/ramplus"
	// Group — группа, которой файл доступен на чтение (пользователь
	// ramplus-web). Владелец — root, права 0640.
	Group    = "ramplus-web"
	FileMode = 0o640
)

// MinPasswordLen — минимальная длина пароля (символов).
const MinPasswordLen = 8

// maxPasswordLen — защита от огромных паролей (argon2 считает их долго).
const maxPasswordLen = 1024

// Params — параметры argon2id.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams — RFC 9106, рекомендация для систем с ограниченной памятью
// (64 MiB, t=3); потоков 2.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// Ошибки.
var (
	ErrFormat      = i18n.Errorf("webauth.err.format")
	ErrPasswordBad = i18n.Errorf("webauth.err.bad")
)

var b64 = base64.RawStdEncoding

// CheckPassword — требования к новому паролю.
func CheckPassword(pw string) error {
	switch {
	case !utf8.ValidString(pw):
		return i18n.Errorf("webauth.err.utf8")
	case strings.ContainsAny(pw, "\r\n\x00"):
		return i18n.Errorf("webauth.err.newline")
	case utf8.RuneCountInString(pw) < MinPasswordLen:
		return i18n.Errorf("webauth.err.short", MinPasswordLen)
	case len(pw) > maxPasswordLen:
		return i18n.Errorf("webauth.err.long", maxPasswordLen)
	case strings.TrimFunc(pw, unicode.IsSpace) == "":
		return i18n.Errorf("webauth.err.spaces")
	}
	return nil
}

// Hash — PHC-строка argon2id со случайной солью.
func Hash(password string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Parse разбирает PHC-строку argon2id.
func Parse(encoded string) (Params, []byte, []byte, error) {
	var p Params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrFormat
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return p, nil, nil, ErrFormat
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, ErrFormat
	}
	// Границы — чтобы испорченный файл не заставил считать часами.
	if p.Memory < 8*1024 || p.Memory > 1024*1024 || p.Time < 1 || p.Time > 20 || p.Threads < 1 {
		return p, nil, nil, ErrFormat
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return p, nil, nil, ErrFormat
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 128 {
		return p, nil, nil, ErrFormat
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}

// Verify — пароль соответствует хешу (сравнение за постоянное время).
func Verify(encoded, password string) (bool, error) {
	p, salt, key, err := Parse(encoded)
	if err != nil {
		return false, err
	}
	if len(password) > maxPasswordLen {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

const fileHeader = "# RamPlus web interface password (argon2id). Change it: sudo ramplus passwd\n"

// FileContent — содержимое файла пароля.
func FileContent(hash string) []byte { return []byte(fileHeader + hash + "\n") }

// ParseFile — хеш из файла пароля (комментарии и пустые строки
// пропускаются).
func ParseFile(b []byte) (string, error) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, _, _, err := Parse(line); err != nil {
			return "", err
		}
		return line, nil
	}
	return "", i18n.Errorf("webauth.err.no_hash")
}
