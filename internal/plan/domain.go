package plan

import (
	"fmt"
	"sync"
	"time"
)

// Домены операций: что меняет операция (для истории, rollback и веба).
const (
	DomainSwap    = "swap"
	DomainProfile = "profile"
)

// DomainNoun — что меняет домен, для человека («а не изменение swap»).
var DomainNoun = map[string]string{
	DomainSwap:    "изменение swap",
	DomainProfile: "настройка профиля",
}

// ActionInfo — домен операции и её название для человека.
type ActionInfo struct {
	Domain string
	Title  string // «создание swap», «сброс профиля»
}

var (
	actionsMu sync.RWMutex
	actions   = map[Action]ActionInfo{}
)

// RegisterAction — домены регистрируют свои операции.
func RegisterAction(a Action, info ActionInfo) {
	actionsMu.Lock()
	defer actionsMu.Unlock()
	actions[a] = info
}

// InfoOf — домен и название операции (неизвестная — пустой домен,
// название — сам код операции).
func InfoOf(a Action) ActionInfo {
	actionsMu.RLock()
	defer actionsMu.RUnlock()
	if i, ok := actions[a]; ok {
		return i
	}
	return ActionInfo{Title: string(a)}
}

// RollbackView — что отменяет план rollback (для JSON/веба).
type RollbackView struct {
	OperationID string    `json:"operation_id"`
	Action      Action    `json:"action"`
	Domain      string    `json:"domain"`
	Command     string    `json:"command"`
	StartedAt   time.Time `json:"started_at"`
	AgeSeconds  int64     `json:"age_seconds"`
}

var monthsGenitive = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

// HumanDate — «17 ноября 14:03» (в местном времени).
func HumanDate(t time.Time) string {
	t = t.Local()
	return fmt.Sprintf("%d %s %s", t.Day(), monthsGenitive[t.Month()-1], t.Format("15:04"))
}

// HumanAge — «только что», «5 минут назад», «3 часа назад», «2 дня назад».
func HumanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s назад", n, plural(n, "минуту", "минуты", "минут"))
	case d < 48*time.Hour:
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s назад", n, plural(n, "час", "часа", "часов"))
	default:
		n := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d %s назад", n, plural(n, "день", "дня", "дней"))
	}
}

// plural — русская форма числительного: 1 час, 2 часа, 5 часов, 21 час.
func plural(n int, one, few, many string) string {
	n %= 100
	if n >= 11 && n <= 14 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	}
	return many
}
