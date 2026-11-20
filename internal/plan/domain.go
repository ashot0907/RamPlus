package plan

import (
	"sync"
	"time"

	"github.com/ashot0907/ramplus/internal/i18n"
)

// Домены операций: что меняет операция (для истории, rollback и веба).
const (
	DomainSwap    = "swap"
	DomainProfile = "profile"
	// DomainSetup — swap и профиль одной операцией (ramplus setup, веб).
	DomainSetup = "setup"
)

// DomainNoun — что меняет домен, для человека («а не изменение swap»).
func DomainNoun(l i18n.Lang, domain string) string { return i18n.T(l, "domain."+domain) }

// ActionInfo — домен операции. Название для человека — сообщение каталога
// "action.<операция>" (ActionTitle).
type ActionInfo struct {
	Domain string
}

// ActionTitle — название операции для человека («создание swap»);
// неизвестная — сам код.
func ActionTitle(l i18n.Lang, a Action) string {
	if i18n.Has("action." + string(a)) {
		return i18n.T(l, "action."+string(a))
	}
	return string(a)
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

// InfoOf — домен операции (неизвестная — пустой домен).
func InfoOf(a Action) ActionInfo {
	actionsMu.RLock()
	defer actionsMu.RUnlock()
	return actions[a]
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

// HumanDate — «17 ноября 14:03» / «November 17 14:03» (в местном времени).
func HumanDate(l i18n.Lang, t time.Time) string {
	t = t.Local()
	return i18n.T(l, "date.day_month_time", t.Day(), i18n.T(l, "date.month."+t.Format("01")), t.Format("15:04"))
}

// HumanAge — «только что», «5 минут назад», «3 часа назад», «2 дня назад».
func HumanAge(l i18n.Lang, d time.Duration) string {
	switch {
	case d < time.Minute:
		return i18n.T(l, "age.now")
	case d < time.Hour:
		return i18n.Plural(l, int(d/time.Minute), "age.minutes")
	case d < 48*time.Hour:
		return i18n.Plural(l, int(d/time.Hour), "age.hours")
	default:
		return i18n.Plural(l, int(d/(24*time.Hour)), "age.days")
	}
}

// ChangeView — что меняет операция: размер swap RamPlus и профиль до и
// после (история, веб). nil-размер — swap RamPlus нет; "" — профиля нет.
type ChangeView struct {
	SwapBefore    *uint64 `json:"swap_before_bytes"`
	SwapAfter     *uint64 `json:"swap_after_bytes"`
	ProfileBefore string  `json:"profile_before"`
	ProfileAfter  string  `json:"profile_after"`
	// Target — rollback: что отменяется.
	Target *ChangeView `json:"target,omitempty"`
}

// ActionText — название операции (переводится при выводе).
func ActionText(a Action) i18n.Text {
	return i18n.Func(func(l i18n.Lang) string { return ActionTitle(l, a) })
}

// DateText — HumanDate на языке вывода.
func DateText(t time.Time) i18n.Text {
	return i18n.Func(func(l i18n.Lang) string { return HumanDate(l, t) })
}

// AgeText — HumanAge на языке вывода.
func AgeText(d time.Duration) i18n.Text {
	return i18n.Func(func(l i18n.Lang) string { return HumanAge(l, d) })
}

// DomainText — DomainNoun на языке вывода.
func DomainText(domain string) i18n.Text { return i18n.Msg("domain." + domain) }
