package render

import (
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/ashot0907/ramplus/internal/journal"
)

var opStatusText = map[string]string{
	journal.OpCommitted:      "выполнена",
	journal.OpRolledBack:     "откачена",
	journal.OpFailed:         "ошибка, откачена",
	journal.OpInProgress:     "прервана",
	journal.OpRollbackFailed: "откат не удался",
}

// History — ramplus history: операции журнала по порядку.
func History(w io.Writer, h journal.History) error {
	if len(h.Operations) == 0 {
		_, err := fmt.Fprintf(w, "Журнал %s пуст: RamPlus ничего не менял.\n", h.Journal)
		return err
	}
	fmt.Fprintf(w, "Журнал: %s (операций: %d)\n\n", h.Journal, len(h.Operations))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tВРЕМЯ\tСТАТУС\tОТКАТ\tОПЕРАЦИЯ\tИСТОЧНИК\tКОМАНДА")
	var errs []journal.HistoryEntry
	for _, op := range h.Operations {
		status := opStatusText[op.Status]
		if status == "" {
			status = op.Status
		}
		undo := "—"
		switch {
		case op.RollbackNext:
			undo = "следующий"
		case op.Undoable:
			undo = "да"
		}
		cmd := op.Command
		if op.Target != "" {
			cmd += " (отменяет " + op.Target + ")"
		}
		title := op.Title
		if title == "" {
			title = "—"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", op.ID, op.StartedAt.Local().Format("2006-01-02 15:04:05"), status, undo, title, SourceText(op), cmd)
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
	fmt.Fprintln(w, "\nОТКАТ: «следующий» — эту операцию отменит ramplus rollback; «да» — повторные rollback дойдут до неё.")
	return nil
}

// SourceText — откуда запрошена операция: «из терминала (ashot)», «через
// веб-интерфейс», «через API демона (uid 1001)», «неизвестно» (старый журнал).
func SourceText(e journal.HistoryEntry) string {
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
		return "через веб-интерфейс"
	case e.Source == journal.SourceCLI:
		return with("из терминала")
	case e.Source == journal.SourceDaemon:
		return with("через API демона")
	}
	return "неизвестно"
}
