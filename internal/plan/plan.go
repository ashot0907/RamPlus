// Package plan — общая модель плана: что будет сделано, почему и что
// выбрал пользователь. План — данные (сериализуется в JSON): CLI, демон и
// веб строят текст и интерфейс только из него.
package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ashot0907/ramplus/internal/i18n"
)

// Action — операция.
type Action string

// ActionRollback — откат последней операции (общий для всех доменов).
const ActionRollback Action = "rollback"

// Level — серьёзность проверки.
type Level string

const (
	LevelOK   Level = "ok"
	LevelInfo Level = "info"
	LevelWarn Level = "warn"
	LevelFail Level = "fail" // блокирует выполнение
)

// Check — результат preflight-проверки.
// Code — стабильный машинный код («fs.unsupported», «rollback.nothing»),
// не зависит от текста; Title и Message — человекочитаемые тексты.
type Check struct {
	Level   Level  `json:"level"`
	Code    string `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// Alternative — один из вариантов выбора и его последствия.
type Alternative struct {
	Value    string `json:"value"`
	Selected bool   `json:"selected"`
	Summary  string `json:"summary"`
}

// Choice — решение пользователя, которое показывается явно.
type Choice struct {
	Title        string        `json:"title"`
	Flag         string        `json:"flag"`
	Value        string        `json:"value"`
	Defaulted    bool          `json:"defaulted"` // выбрано по умолчанию, флаг не указан
	Note         string        `json:"note,omitempty"`
	Alternatives []Alternative `json:"alternatives,omitempty"`
}

// StepKind — тип системного действия.
type StepKind string

// Общие шаги (файлы и systemd); доменные регистрируют свои kind.
const (
	KindWriteFile    StepKind = "write_file"    // записать Content в Path
	KindDeleteConfig StepKind = "delete_config" // удалить конфиг; откат восстанавливает содержимое
	KindSystemctl    StepKind = "systemctl"     // systemctl Args...
	KindMkdir        StepKind = "mkdir"
	KindRmdir        StepKind = "rmdir"
	KindChmod        StepKind = "chmod" // chmod Mode Path
)

// Step — одно системное действие. Сериализуется в журнал (state.json):
// набор полей и их JSON-имена — часть формата журнала.
type Step struct {
	Kind     StepKind `json:"kind"`
	Path     string   `json:"path,omitempty"`
	Size     uint64   `json:"size,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Mode     uint32   `json:"mode,omitempty"`
	Content  string   `json:"content,omitempty"`
	Line     string   `json:"line,omitempty"`
	Args     []string `json:"args,omitempty"`
	// NoCOW — swapfile на btrfs: перед выделением места файлу ставится
	// chattr +C (иначе ядро откажет в swapon).
	NoCOW bool `json:"nocow,omitempty"`
	// Reload — после отмены шага нужен systemctl daemon-reload (шаг меняет
	// конфигурацию systemd: юнит или fstab).
	Reload bool `json:"reload,omitempty"`
	// SizeAtMost — delete_swapfile: файл может быть меньше Size (удаление
	// недописанного файла после падения посреди allocate).
	SizeAtMost bool `json:"size_at_most,omitempty"`
}

// DaemonReload — шаг systemctl daemon-reload.
func DaemonReload() Step { return Step{Kind: KindSystemctl, Args: []string{"daemon-reload"}} }

// Describer — описание шага для человека (сообщение каталога).
type Describer func(Step) i18n.Text

var (
	describersMu sync.RWMutex
	describers   = map[StepKind]Describer{}
)

// RegisterDescriber задаёт описание шагов вида kind для человека.
func RegisterDescriber(kind StepKind, fn Describer) {
	describersMu.Lock()
	defer describersMu.Unlock()
	describers[kind] = fn
}

// Description — описание шага (переводится при выводе).
func (s Step) Description() i18n.Text {
	describersMu.RLock()
	fn := describers[s.Kind]
	describersMu.RUnlock()
	if fn != nil {
		return fn(s)
	}
	return i18n.Raw(string(s.Kind) + " " + s.Path)
}

// Describe — описание шага на языке l.
func (s Step) Describe(l i18n.Lang) string { return s.Description().Text(l) }

func init() {
	RegisterDescriber(KindWriteFile, func(s Step) i18n.Text { return i18n.Msg("step.write_file", s.Path) })
	RegisterDescriber(KindDeleteConfig, func(s Step) i18n.Text { return i18n.Msg("step.delete_config", s.Path) })
	RegisterDescriber(KindSystemctl, func(s Step) i18n.Text { return i18n.Raw("systemctl " + strings.Join(s.Args, " ")) })
	RegisterDescriber(KindMkdir, func(s Step) i18n.Text { return i18n.Msg("step.mkdir", s.Path) })
	RegisterDescriber(KindRmdir, func(s Step) i18n.Text { return i18n.Msg("step.rmdir", s.Path) })
	RegisterDescriber(KindChmod, func(s Step) i18n.Text { return i18n.Raw(fmt.Sprintf("chmod %o %s", s.Mode, s.Path)) })
}

// Summary — итог плана.
type Summary struct {
	SwapBefore uint64   `json:"swap_before_bytes"`
	SwapAfter  uint64   `json:"swap_after_bytes"`
	Created    []string `json:"created"`
	Disabled   []string `json:"disabled"`
	Deleted    []string `json:"deleted"`
}

// Target — цель rollback: позиция в стеке журнала и ID.
type Target struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
}

// Sections — разделы журнала, которые операция задаёт после коммита
// (содержимое непрозрачно для журнала; nil — null/нет).
type Sections struct {
	Managed  json.RawMessage
	Profile  json.RawMessage
	Baseline json.RawMessage
}

// Effects — что журнал запишет при коммите операции.
type Effects struct {
	Managed json.RawMessage // раздел managed после операции
	Profile json.RawMessage // раздел profile после операции
	// BaselineCapture — снять исходные значения (если их ещё нет).
	BaselineCapture json.RawMessage
	Target          *Target // rollback
	// Finalize проставляет время и ID операции в разделы перед записью.
	Finalize func(s *Sections, now time.Time, opID string, baselineCaptured bool)
}

// Plan — что будет сделано и почему.
type Plan struct {
	// Lang — язык текстов проверок и описаний шагов (задаёт тот, кто строит
	// план; коды от языка не зависят).
	Lang i18n.Lang `json:"-"`
	// ID — идентификатор построенного плана (задаёт core; по нему Apply).
	ID string `json:"id,omitempty"`
	// Fingerprint — отпечаток фактов, на которых построен план (Seal).
	Fingerprint string `json:"fingerprint"`

	Action  Action   `json:"action"`
	Command string   `json:"command"`
	Choices []Choice `json:"choices"`
	Checks  []Check  `json:"checks"`
	Steps   []Step   `json:"steps"`
	Summary
	Swap    *SwapView    `json:"swap,omitempty"`
	Profile *ProfileView `json:"profile,omitempty"`
	// Rollback — что отменяет план rollback.
	Rollback *RollbackView `json:"rollback,omitempty"`
	Effects  Effects       `json:"-"`
}

// Blocked — есть проваленные проверки.
func (p Plan) Blocked() bool { return len(p.Failures()) > 0 }

// Failures — проваленные проверки.
func (p Plan) Failures() []Check {
	var out []Check
	for _, c := range p.Checks {
		if c.Level == LevelFail {
			out = append(out, c)
		}
	}
	return out
}

// Add добавляет проверку с машинным кодом code. Заголовок — сообщение
// каталога "check.<code>.title", текст — "check.<code>" с параметрами a.
func (p *Plan) Add(l Level, code string, a ...any) {
	p.AddMsg(l, code, i18n.Msg("check."+code+".title"), i18n.Msg("check."+code, a...))
}

// AddKey — то же, текст по ключу "check.<key>" (у кода несколько вариантов
// текста: например, с --force и без).
func (p *Plan) AddKey(l Level, code, key string, a ...any) {
	p.AddMsg(l, code, i18n.Msg("check."+code+".title"), i18n.Msg("check."+key, a...))
}

// AddMsg — проверка с заголовком title и текстом msg на языке плана.
func (p *Plan) AddMsg(l Level, code string, title, msg i18n.Text) {
	p.Checks = append(p.Checks, Check{Level: l, Code: code, Title: title.Text(p.Lang), Message: msg.Text(p.Lang)})
}

// T — сообщение каталога на языке плана.
func (p *Plan) T(key string, a ...any) string { return i18n.T(p.Lang, key, a...) }

// HasKind — есть ли в плане шаг вида k.
func (p Plan) HasKind(k StepKind) bool {
	for _, s := range p.Steps {
		if s.Kind == k {
			return true
		}
	}
	return false
}
