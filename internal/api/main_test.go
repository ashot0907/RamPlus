package api

import (
	"os"
	"testing"

	"github.com/ashot0907/ramplus/internal/i18n"
)

// Эталоны тестов пакета — на русском; английский проверяется отдельно.
func TestMain(m *testing.M) {
	i18n.Default = i18n.RU
	os.Exit(m.Run())
}
