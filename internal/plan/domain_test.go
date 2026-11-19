package plan

import (
	"testing"
	"time"
)

func TestHumanAge(t *testing.T) {
	tests := map[time.Duration]string{
		10 * time.Second:    "только что",
		time.Minute:         "1 минуту назад",
		22 * time.Minute:    "22 минуты назад",
		11 * time.Minute:    "11 минут назад",
		time.Hour:           "1 час назад",
		3 * time.Hour:       "3 часа назад",
		21 * time.Hour:      "21 час назад",
		47 * time.Hour:      "47 часов назад",
		50 * time.Hour:      "2 дня назад",
		5 * 24 * time.Hour:  "5 дней назад",
		21 * 24 * time.Hour: "21 день назад",
	}
	for d, want := range tests {
		if got := HumanAge(d); got != want {
			t.Errorf("HumanAge(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestHumanDate(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()
	if got := HumanDate(time.Date(2026, 11, 17, 14, 3, 0, 0, time.UTC)); got != "17 ноября 14:03" {
		t.Errorf("got %q", got)
	}
}
