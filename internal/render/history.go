package render

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/journal"
)

// opStatusText — статус операции для человека ("" — неизвестный).
func opStatusText(l i18n.Lang, status string) string {
	switch status {
	case journal.OpCommitted, journal.OpRolledBack, journal.OpFailed, journal.OpInProgress, journal.OpRollbackFailed:
		return t(l, "render.op_status."+status)
	}
	return ""
}

// History — ramplus history: операции журнала по порядку.
func History(l i18n.Lang, w io.Writer, h journal.History) error {
	if len(h.Operations) == 0 {
		_, err := fmt.Fprintf(w, t(l, "render.history.empty"), h.Journal)
		return err
	}
	fmt.Fprintf(w, t(l, "render.history.header"), h.Journal, len(h.Operations))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, t(l, "render.history.columns"))
	var errs []journal.HistoryEntry
	for _, op := range h.Operations {
		status := opStatusText(l, op.Status)
		if status == "" {
			status = op.Status
		}
		undo := "—"
		switch {
		case op.RollbackNext:
			undo = t(l, "render.history.undo_next")
		case op.Undoable:
			undo = t(l, "render.yes")
		}
		cmd := op.Command
		if op.Target != "" {
			cmd += t(l, "render.history.undoes") + op.Target + ")"
		}
		title := op.Title
		if title == "" {
			title = "—"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", op.ID, op.StartedAt.Local().Format("2006-01-02 15:04:05"), status, undo, title, SourceText(l, op), cmd)
		if op.Error != "" {
			errs = append(errs, op)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, op := range errs {
		fmt.Fprintf(w, "\n%s: %s\n", op.ID, op.Error)
	}
	fmt.Fprintln(w, t(l, "render.history.legend"))
	return nil
}

// SourceText — откуда запрошена операция: «из терминала (ashot)», «через
// веб-интерфейс», «через API демона (uid 1001)», «неизвестно» (старый журнал).
func SourceText(l i18n.Lang, e journal.HistoryEntry) string {
	who := ""
	if r := e.RequestedBy; r != nil {
		who = r.User
		if who == "" {
			who = "uid " + strconv.FormatUint(uint64(r.UID), 10)
		}
	}
	with := func(s string) string {
		if who == "" {
			return s
		}
		return s + " (" + who + ")"
	}
	switch {
	case e.Source == journal.SourceDaemon && e.RequestedBy != nil && e.RequestedBy.Via == journal.ViaWeb:
		return t(l, "render.source.web")
	case e.Source == journal.SourceCLI:
		return with(t(l, "render.source.cli"))
	case e.Source == journal.SourceDaemon:
		return with(t(l, "render.source.daemon"))
	}
	return t(l, "render.unknown")
}
