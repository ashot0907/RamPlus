package core

import (
	"os"
	"testing"

	"github.com/ashot0907/ramplus/internal/i18n"
)

// Эталоны тестов пакета — на русском (тексты по коду из каталога i18n;
// английский проверяется golden-тестами CLI и тестом каталога).
func TestMain(m *testing.M) {
	i18n.Default = i18n.RU
	os.Exit(m.Run())
}
