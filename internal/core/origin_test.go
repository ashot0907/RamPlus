package core

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"context"
	"testing"

	"github.com/ashot0907/ramplus/internal/journal"
)

func names(uid uint32) string {
	return map[uint32]string{0: "root", 1000: "ashot", 997: "ramplus-web"}[uid]
}

func TestCLIOrigin(t *testing.T) {
	cases := []struct {
		uid               int
		sudoUID, sudoUser string
		want              journal.Requester
	}{
		{0, "1000", "ashot", journal.Requester{UID: 1000, User: "ashot"}}, // sudo ramplus …
		{0, "1000", "", journal.Requester{UID: 1000, User: "ashot"}},      // имя — из системы
		{0, "", "", journal.Requester{UID: 0, User: "root"}},              // root без sudo
		{0, "x", "ashot", journal.Requester{UID: 0, User: "root"}},        // мусор в SUDO_UID
		{1000, "5", "evil", journal.Requester{UID: 1000, User: "ashot"}},  // не root: SUDO_* не доверяем
		{4242, "", "", journal.Requester{UID: 4242}},                      // нет в /etc/passwd
	}
	for _, c := range cases {
		o := CLIOrigin(c.uid, c.sudoUID, c.sudoUser, names)
		if o.Source != journal.SourceCLI || *o.RequestedBy != c.want {
			t.Errorf("CLIOrigin(%d, %q, %q) = %s %+v, want %+v", c.uid, c.sudoUID, c.sudoUser, o.Source, *o.RequestedBy, c.want)
		}
	}
}

func TestDaemonRequester(t *testing.T) {
	web := uint32(997)
	if r := DaemonRequester(997, &web, names); r.Via != journal.ViaWeb || r.User != "ramplus-web" {
		t.Errorf("web: %+v", r)
	}
	if r := DaemonRequester(1000, &web, names); r.Via != "" || r.User != "ashot" {
		t.Errorf("admin: %+v", r)
	}
	if r := DaemonRequester(997, nil, names); r.Via != "" {
		t.Errorf("no web user configured: %+v", r)
	}
}

// Источник записывается в журнал: CLI (Execute) и демон (Start).
func TestOriginInJournal(t *testing.T) {
	w := setupWorld(t)
	c := New(w.System())
	h := journal.Hooks{Confirm: yes, Origin: CLIOrigin(0, "1000", "ashot", names)}
	if res := c.Execute(context.Background(), Request{Swap: &apply4G}, h); res.Status != ResultDone {
		t.Fatal(res.Status, res.Error)
	}
	web := uint32(997)
	p := c.RollbackPlan(context.Background(), i18n.RU)
	op, res := c.Start(p.ID, StartOptions{RequestedBy: DaemonRequester(997, &web, names)})
	if op == nil {
		t.Fatalf("start: %+v", res)
	}
	c.Wait()
	st := stateOf(t, w)
	if len(st.Operations) != 2 {
		t.Fatalf("ops: %d", len(st.Operations))
	}
	cli, dmn := st.Operations[0], st.Operations[1]
	if cli.Source != journal.SourceCLI || cli.RequestedBy == nil || cli.RequestedBy.UID != 1000 || cli.RequestedBy.User != "ashot" {
		t.Errorf("cli op: %s %+v", cli.Source, cli.RequestedBy)
	}
	if dmn.Source != journal.SourceDaemon || dmn.RequestedBy == nil || dmn.RequestedBy.UID != 997 || dmn.RequestedBy.Via != journal.ViaWeb {
		t.Errorf("daemon op: %s %+v", dmn.Source, dmn.RequestedBy)
	}
	hist, _ := c.History(i18n.RU)
	if hist.Operations[0].Source != "cli" || hist.Operations[1].Source != "daemon" || hist.Operations[1].RequestedBy.Via != "web" {
		t.Errorf("history: %+v", hist.Operations)
	}
}
