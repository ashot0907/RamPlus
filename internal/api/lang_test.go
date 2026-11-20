package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ashot0907/ramplus/internal/fakesys"
	"github.com/ashot0907/ramplus/internal/plan"
)

// Язык текстов — из Accept-Language (по умолчанию английский); коды
// ошибок и проверок от языка не зависят.
func TestAcceptLanguage(t *testing.T) {
	h := newHarness(t, fakesys.New().WithOldSwap(0))
	get := func(method, path, body, lang string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://ramplusd"+path, strings.NewReader(body))
		if lang != "" {
			req.Header.Set("Accept-Language", lang)
		}
		resp, err := h.clients["reader"].Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	for lang, want := range map[string]string{"": "no such endpoint", "en-US,en;q=0.9": "no such endpoint", "ru-RU,ru;q=0.9": "нет такого эндпоинта"} {
		code, b := get("GET", "/v1/nope", "", lang)
		var e Error
		_ = json.Unmarshal(b, &e)
		if code != 404 || e.Code != "request.not_found" || !strings.Contains(e.Message, want) {
			t.Errorf("%q: %d %s", lang, code, b)
		}
	}
	plans := map[string]plan.Plan{}
	for _, lang := range []string{"en", "ru"} {
		code, b := get("POST", "/v1/plan/swap", `{"action":"apply","size":"4G"}`, lang)
		var p struct {
			Checks []plan.Check `json:"checks"`
			Steps  []struct {
				Kind        string `json:"kind"`
				Description string `json:"description"`
			} `json:"steps"`
		}
		if code != 200 || json.Unmarshal(b, &p) != nil {
			t.Fatalf("%s: %d %s", lang, code, b)
		}
		plans[lang] = plan.Plan{Checks: p.Checks}
		if lang == "en" && !strings.Contains(p.Steps[0].Description, "create /ramplus.swapfile") {
			t.Errorf("en step: %q", p.Steps[0].Description)
		}
		if lang == "ru" && !strings.Contains(p.Steps[0].Description, "создать /ramplus.swapfile") {
			t.Errorf("ru step: %q", p.Steps[0].Description)
		}
	}
	en, ru := plans["en"].Checks, plans["ru"].Checks
	if len(en) != len(ru) {
		t.Fatalf("checks differ: %d vs %d", len(en), len(ru))
	}
	for i := range en {
		if en[i].Code != ru[i].Code || en[i].Level != ru[i].Level || en[i].Title+en[i].Message == ru[i].Title+ru[i].Message {
			t.Errorf("check %d: %+v vs %+v", i, en[i], ru[i])
		}
	}
}
