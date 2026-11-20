package core

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/webauth"
)

// Ошибки ramplus init / passwd.
var (
	ErrWebPasswordSet    = errors.New("пароль веб-интерфейса уже задан — сменить: sudo ramplus passwd")
	ErrWebPasswordNotSet = errors.New("пароль веб-интерфейса не задан — задать: sudo ramplus init")
)

// WebPasswordMode — что делает команда.
type WebPasswordMode string

const (
	WebPasswordInit    WebPasswordMode = "init"    // задать (файла ещё нет)
	WebPasswordChange  WebPasswordMode = "change"  // сменить (файл есть)
	WebPasswordDisable WebPasswordMode = "disable" // удалить файл: вход в веб невозможен (откат init)
)

// WebPasswordRequest — ramplus init / passwd.
type WebPasswordRequest struct {
	Mode     WebPasswordMode
	Password string // init/change
	GID      int    // группа ramplus-web (владелец-группа файла)
	Path     string // "" — webauth.DefaultPath
	DryRun   bool
	// Params — параметры argon2id (тесты задают дешёвые; nil — по умолчанию).
	Params *webauth.Params
}

// WebPasswordResult — что сделано (или будет сделано при DryRun).
type WebPasswordResult struct {
	Path    string `json:"path"`
	Action  string `json:"action"` // write | remove
	Mode    string `json:"mode"`   // 0640
	Owner   string `json:"owner"`  // root:<gid>
	DryRun  bool   `json:"dry_run"`
	Existed bool   `json:"existed"`
}

// WebPassword — задать, сменить или удалить пароль веб-интерфейса. Хеш
// argon2id пишется атомарно (временный файл + rename) с правами 0640
// root:ramplus-web. Пароль в журнал RamPlus не попадает (журнал читают
// все), поэтому отката через rollback нет: откат init — passwd --disable.
func (c *Core) WebPassword(r WebPasswordRequest) (WebPasswordResult, error) {
	path := r.Path
	if path == "" {
		path = webauth.DefaultPath
	}
	res := WebPasswordResult{Path: path, Action: "write", Mode: fmt.Sprintf("%04o", webauth.FileMode),
		Owner: fmt.Sprintf("root:%d", r.GID), DryRun: r.DryRun}
	_, err := c.Sys.FS.Stat(path)
	res.Existed = err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%s: %w", path, err)
	}
	switch r.Mode {
	case WebPasswordInit:
		if res.Existed {
			return res, ErrWebPasswordSet
		}
	case WebPasswordChange:
		if !res.Existed {
			return res, ErrWebPasswordNotSet
		}
	case WebPasswordDisable:
		res.Action = "remove"
		if !res.Existed {
			return res, ErrWebPasswordNotSet
		}
	default:
		return res, fmt.Errorf("неизвестный режим %q", r.Mode)
	}
	if r.Mode != WebPasswordDisable {
		if err := webauth.CheckPassword(r.Password); err != nil {
			return res, err
		}
	}
	if r.DryRun {
		return res, nil
	}
	if c.Sys.Kernel.Geteuid() != 0 {
		return res, journal.ErrNeedRoot
	}
	if r.Mode == WebPasswordDisable {
		return res, c.Sys.FS.Remove(path)
	}
	params := webauth.DefaultParams
	if r.Params != nil {
		params = *r.Params
	}
	hash, err := webauth.Hash(r.Password, params)
	if err != nil {
		return res, err
	}
	if err := c.Sys.FS.MkdirAll(webauth.Dir, 0o755); err != nil {
		return res, err
	}
	// Временный файл создаётся root:root 0640 — до chown он никому не виден.
	if err := c.Sys.FS.WriteFileAtomic(path, webauth.FileContent(hash), webauth.FileMode); err != nil {
		return res, err
	}
	if err := c.Sys.FS.Chown(path, 0, r.GID); err != nil {
		return res, fmt.Errorf("chown %s: %w", path, err)
	}
	return res, nil
}
