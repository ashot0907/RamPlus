package journal

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ashot0907/ramplus/internal/i18n"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/sys"
)

// Статусы записи журнала (write-ahead).
const (
	StepPending = "pending" // записано до выполнения шага
	StepDone    = "done"    // шаг выполнен
)

// Applied — запись журнала о шаге и о том, как его отменить. Undo
// вычисляется ДО выполнения (например, сохраняется прежнее содержимое
// файла) и пишется вместе со статусом pending: если процесс упадёт посреди
// шага, rollback знает, что проверять и как откатывать.
type Applied struct {
	Step   plan.Step   `json:"step"`
	Status string      `json:"status"`
	Undo   []plan.Step `json:"undo"`
	Backup string      `json:"backup,omitempty"` // копия файла до изменения
}

// UndoSteps — шаги отмены выполненных (done) шагов, в обратном порядке.
func UndoSteps(applied []Applied) []plan.Step {
	var out []plan.Step
	for i := len(applied) - 1; i >= 0; i-- {
		if applied[i].Status == StepDone {
			out = append(out, applied[i].Undo...)
		}
	}
	return out
}

// Env — окружение выполнения шага.
type Env struct {
	Sys  sys.System
	OpID string
}

// Run — внешняя команда.
func (e *Env) Run(ctx context.Context, name string, args ...string) error {
	_, err := e.Sys.Exec.Run(ctx, name, args...)
	return err
}

// Perm — права файла или def, если файла нет.
func (e *Env) Perm(path string, def fs.FileMode) fs.FileMode {
	if st, err := e.Sys.FS.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return def
}

// Backup сохраняет копию файла перед изменением в BackupDir.
func (e *Env) Backup(path string, content []byte) (string, error) {
	if err := e.Sys.FS.MkdirAll(BackupDir, 0o755); err != nil {
		return "", err
	}
	name := strings.ReplaceAll(strings.Trim(path, "/"), "/", "_")
	dst := filepath.Join(BackupDir, e.OpID+"-"+name)
	if err := e.Sys.FS.WriteFileAtomic(dst, content, 0o600); err != nil {
		return "", err
	}
	return dst, nil
}

// Handler — как выполнять, отменять и проверять шаги одного вида.
type Handler struct {
	Describe plan.Describer
	// Prepare читает состояние до шага и вычисляет отмену; систему не
	// меняет (кроме копии в BackupDir). nil — отменять нечего.
	Prepare func(e *Env, s plan.Step) (undo []plan.Step, backup string, err error)
	Do      func(ctx context.Context, e *Env, s plan.Step) error
	// Verify — что осталось от шага, прерванного на середине (pending).
	// nil — отменять нечего.
	Verify func(e *Env, a Applied) (Verdict, i18n.Text)
	// Expect — каким должно быть состояние после шага (сверка перед rollback).
	Expect func(s plan.Step) []Expectation
	// Recovered — поправка шага отмены прерванного шага (например, файл мог
	// остаться недописанным).
	Recovered func(plan.Step) plan.Step
}

var (
	handlersMu sync.RWMutex
	handlers   = map[plan.StepKind]Handler{}
)

// Register регистрирует обработчик шагов вида kind.
func Register(kind plan.StepKind, h Handler) {
	handlersMu.Lock()
	defer handlersMu.Unlock()
	handlers[kind] = h
	if h.Describe != nil {
		plan.RegisterDescriber(kind, h.Describe)
	}
}

func handler(kind plan.StepKind) (Handler, bool) {
	handlersMu.RLock()
	defer handlersMu.RUnlock()
	h, ok := handlers[kind]
	return h, ok
}

// Recovered — шаг отмены прерванного шага с поправкой обработчика.
func Recovered(s plan.Step) plan.Step {
	if h, ok := handler(s.Kind); ok && h.Recovered != nil {
		return h.Recovered(s)
	}
	return s
}

func unknown(kind plan.StepKind) error { return i18n.Errorf("journal.unknown_step", string(kind)) }
