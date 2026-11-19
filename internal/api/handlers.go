package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

// Error — тело ответа с ошибкой: стабильный код и текст для человека.
type Error struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	Plan    *plan.Plan `json:"plan,omitempty"` // plan.blocked: план с проваленными проверками
}

// Health — GET /v1/health.
type Health struct {
	Version       string    `json:"version"`
	API           string    `json:"api"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	PID           int       `json:"pid"`
	Root          bool      `json:"root"` // демон работает от root (иначе apply откажет)
}

// SwapPlanRequest — POST /v1/plan/swap.
type SwapPlanRequest struct {
	Action    swap.Action `json:"action"` // apply | resize | remove
	Size      Size        `json:"size"`   // "4G" или число байт
	Existing  string      `json:"existing"`
	DeleteOld bool        `json:"delete_old"`
	Path      string      `json:"path"`
	Force     bool        `json:"force"`
	Orphan    bool        `json:"orphan"` // remove: следы RamPlus без записи в журнале
}

// ProfilePlanRequest — POST /v1/plan/profile.
type ProfilePlanRequest struct {
	Name         string            `json:"name"`
	Set          map[string]string `json:"set"`
	Experimental bool              `json:"experimental"`
	Force        bool              `json:"force"`
}

// RollbackPlanRequest — POST /v1/plan/rollback (тело необязательно).
type RollbackPlanRequest struct {
	Force bool `json:"force"`
}

// ApplyRequest — POST /v1/apply.
type ApplyRequest struct {
	PlanID              string `json:"plan_id"`
	ConfirmExperimental bool   `json:"confirm_experimental"`
}

// ApplyAccepted — 202 на POST /v1/apply.
type ApplyAccepted struct {
	OperationID string `json:"operation_id"`
	PlanID      string `json:"plan_id"`
	Status      string `json:"status"` // running
	Self        string `json:"self"`
	Events      string `json:"events"`
}

// Size — размер swap: строка с единицами («4G», «512M») или число байт.
type Size uint64

// UnmarshalJSON принимает строку или число.
func (s *Size) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		if str == "" {
			*s = 0
			return nil
		}
		v, err := swap.ParseSize(str)
		if err != nil {
			return err
		}
		*s = Size(v)
		return nil
	}
	var n uint64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("size: ожидается строка (\"4G\") или число байт")
	}
	*s = Size(n)
	return nil
}

func (s *Server) routes() {
	m := http.NewServeMux()
	handle := func(pattern string, a Access, h http.HandlerFunc) { m.Handle(pattern, s.authorize(a, h)) }
	handle("GET /v1/health", AccessAny, s.health)
	handle("GET /v1/status", AccessRead, s.status)
	handle("GET /v1/status/stream", AccessRead, s.statusStream)
	handle("GET /v1/history", AccessRead, s.history)
	handle("POST /v1/plan/swap", AccessRead, s.planSwap)
	handle("POST /v1/plan/profile", AccessRead, s.planProfile)
	handle("POST /v1/plan/profile-reset", AccessRead, s.planProfileReset)
	handle("POST /v1/plan/rollback", AccessRead, s.planRollback)
	handle("POST /v1/apply", AccessAdmin, s.apply)
	handle("GET /v1/operations/{id}", AccessRead, s.operation)
	handle("GET /v1/operations/{id}/events", AccessRead, s.operationEvents)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			r2 := r.Clone(r.Context())
			r2.Method = method
			if _, pat := m.Handler(r2); pat != "/" && pat != "" {
				writeError(w, http.StatusMethodNotAllowed, "request.method_not_allowed", "метод "+r.Method+" не поддерживается для "+r.URL.Path)
				return
			}
		}
		writeError(w, http.StatusNotFound, "request.not_found", "нет такого эндпоинта: "+r.URL.Path)
	})
	s.mux = m
}

// authorize — проверка доступа по SO_PEERCRED; отказ — 403 и запись в лог.
func (s *Server) authorize(a Access, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := PeerFrom(r.Context())
		if err != nil || !s.Policy.Allow(p, a) {
			s.Log.Warn("auth.forbidden", "uid", p.UID, "gid", p.GID, "groups", p.Groups, "pid", p.PID,
				"method", r.Method, "path", r.URL.Path, "need", a.String(), "peer_error", errText(err))
			msg := "нет прав: чтение — root или группа ramplus, изменения — root или группа ramplus-admin"
			writeError(w, http.StatusForbidden, "auth.forbidden", msg)
			return
		}
		h(w, r)
	})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, Error{Code: errCode, Message: msg})
}

// decode читает JSON-тело (не больше MaxRequestBytes, без неизвестных
// полей). Пустое тело — нулевое значение. false — ответ с ошибкой уже
// записан.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.More() {
		err = errors.New("лишние данные после JSON")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return true
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request.too_large", fmt.Sprintf("тело запроса больше %d байт", MaxRequestBytes))
	default:
		writeError(w, http.StatusBadRequest, "request.invalid", "неверный JSON: "+err.Error())
	}
	return false
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{Version: s.Version, API: APIVersion, StartedAt: s.started,
		UptimeSeconds: int64(time.Since(s.started) / time.Second), PID: os.Getpid(), Root: s.Core.Sys.Kernel.Geteuid() == 0})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Core.Status())
}

func (s *Server) history(w http.ResponseWriter, _ *http.Request) {
	h, err := s.Core.History()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "journal.unreadable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) planSwap(w http.ResponseWriter, r *http.Request) {
	var req SwapPlanRequest
	if !decode(w, r, &req) {
		return
	}
	switch req.Action {
	case swap.ActionApply, swap.ActionResize, swap.ActionRemove:
	default:
		writeError(w, http.StatusBadRequest, "request.invalid", fmt.Sprintf("action: ожидается apply, resize или remove, получено %q", req.Action))
		return
	}
	switch swap.ExistingMode(req.Existing) {
	case "", swap.ExistingKeep, swap.ExistingReplace:
	default:
		writeError(w, http.StatusBadRequest, "request.invalid", fmt.Sprintf("existing: ожидается keep или replace, получено %q", req.Existing))
		return
	}
	o := swap.Options{Action: req.Action, Size: uint64(req.Size), Existing: swap.ExistingMode(req.Existing),
		DeleteOld: req.DeleteOld, Path: req.Path, Force: req.Force, Orphan: req.Orphan}
	writeJSON(w, http.StatusOK, s.Core.SwapPlan(r.Context(), o))
}

func (s *Server) planProfile(w http.ResponseWriter, r *http.Request) {
	var req ProfilePlanRequest
	if !decode(w, r, &req) {
		return
	}
	pairs := make([]string, 0, len(req.Set))
	for k, v := range req.Set {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	set, err := profile.ParseSet(pairs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "request.invalid", "set: "+err.Error())
		return
	}
	pr := profile.ProfileRequest{Name: req.Name, Set: set, Force: req.Force, Experimental: req.Experimental}
	writeJSON(w, http.StatusOK, s.Core.ProfilePlan(r.Context(), pr))
}

func (s *Server) planProfileReset(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.Core.ProfileResetPlan(r.Context()))
}

func (s *Server) planRollback(w http.ResponseWriter, r *http.Request) {
	var req RollbackPlanRequest
	if !decode(w, r, &req) {
		return
	}
	o := swap.Options{Action: plan.ActionRollback, Force: req.Force}
	writeJSON(w, http.StatusOK, s.Core.SwapPlan(r.Context(), o))
}

// resultError — HTTP-код и код ошибки для итога, с которым операция не
// началась.
var resultError = map[core.ResultStatus]struct {
	http int
	code string
}{
	core.ResultUnknownPlan:  {http.StatusNotFound, "plan.not_found"},
	core.ResultPlanUsed:     {http.StatusConflict, "plan.used"},
	core.ResultStale:        {http.StatusConflict, "plan.stale"},
	core.ResultBusy:         {http.StatusConflict, "op.busy"},
	core.ResultLocked:       {http.StatusConflict, "op.locked"},
	core.ResultBlocked:      {http.StatusUnprocessableEntity, "plan.blocked"},
	core.ResultNotConfirmed: {http.StatusUnprocessableEntity, "profile.experimental_not_confirmed"},
	core.ResultCancelled:    {http.StatusConflict, "op.cancelled"},
	core.ResultNeedRoot:     {http.StatusInternalServerError, "daemon.not_root"},
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	var req ApplyRequest
	if !decode(w, r, &req) {
		return
	}
	if req.PlanID == "" {
		writeError(w, http.StatusBadRequest, "request.invalid", "plan_id: обязательное поле (POST /v1/plan/...)")
		return
	}
	op, res := s.Core.Start(req.PlanID, core.StartOptions{ConfirmExperimental: req.ConfirmExperimental})
	if op != nil {
		p, _ := PeerFrom(r.Context())
		s.Log.Info("operation.started", "id", op.ID, "plan_id", req.PlanID, "command", op.Command, "uid", p.UID, "pid", p.PID)
		self := "/v1/operations/" + op.ID
		writeJSON(w, http.StatusAccepted, ApplyAccepted{OperationID: op.ID, PlanID: req.PlanID, Status: string(core.StatusRunning),
			Self: self, Events: self + "/events"})
		return
	}
	if res.Status == core.ResultNothing {
		writeJSON(w, http.StatusOK, res)
		return
	}
	e, ok := resultError[res.Status]
	if !ok {
		e.http, e.code = http.StatusInternalServerError, "op.failed"
	}
	body := Error{Code: e.code, Message: res.Error}
	if res.Status == core.ResultBlocked {
		body.Plan = res.Plan
	}
	writeJSON(w, e.http, body)
}

func (s *Server) lookupOp(w http.ResponseWriter, r *http.Request) (*core.Operation, bool) {
	op, err := s.Core.Operation(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "op.not_found", "операция "+strconv.Quote(r.PathValue("id"))+" не найдена (демон помнит только запущенные им операции; история — GET /v1/history)")
		return nil, false
	}
	return op, true
}

func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	if op, ok := s.lookupOp(w, r); ok {
		writeJSON(w, http.StatusOK, op.View())
	}
}
