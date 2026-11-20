package core

import (
	"strconv"

	"github.com/ashot0907/ramplus/internal/journal"
)

// CLIOrigin — источник операции, запущенной из терминала: uid того, кто
// вызвал sudo (SUDO_UID/SUDO_USER), иначе свой uid. lookup — имя по uid
// ("" — нет в системе).
func CLIOrigin(uid int, sudoUID, sudoUser string, lookup func(uint32) string) journal.Origin {
	r := &journal.Requester{UID: uint32(uid)}
	if uid == 0 && sudoUID != "" {
		if v, err := strconv.ParseUint(sudoUID, 10, 32); err == nil {
			r.UID, r.User = uint32(v), sudoUser
		}
	}
	if r.User == "" && lookup != nil {
		r.User = lookup(r.UID)
	}
	return journal.Origin{Source: journal.SourceCLI, RequestedBy: r}
}

// DaemonRequester — кто запросил операцию через демон: uid по SO_PEERCRED;
// запрос от пользователя веб-интерфейса (webUID) помечается via=web.
func DaemonRequester(uid uint32, webUID *uint32, lookup func(uint32) string) *journal.Requester {
	r := &journal.Requester{UID: uid}
	if lookup != nil {
		r.User = lookup(uid)
	}
	if webUID != nil && *webUID == uid {
		r.Via = journal.ViaWeb
	}
	return r
}
