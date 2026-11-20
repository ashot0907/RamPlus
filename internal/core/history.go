package core

import (
	"github.com/ashot0907/ramplus/internal/i18n"

	"bytes"
	"strings"

	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/profile"
	"github.com/ashot0907/ramplus/internal/swap"
)

// History — операции из журнала (ID, время, команда, статус, можно ли
// откатить, название для человека). Только чтение, root не нужен; нет
// журнала — пустая история.
func (c *Core) History(l i18n.Lang) (journal.History, error) {
	st, err := journal.LoadState(c.Sys)
	if err != nil {
		return journal.HistoryOf(journal.State{}), err
	}
	h := journal.HistoryOf(st)
	byID := map[string]journal.Operation{}
	for _, op := range st.Operations {
		byID[op.ID] = op
	}
	for i := range h.Operations {
		op := byID[h.Operations[i].ID]
		ch := changeOf(op)
		if op.Action == plan.ActionRollback {
			if t, ok := byID[op.Target]; ok {
				tc := changeOf(t)
				ch.Target = &tc
				h.Operations[i].Title = i18n.T(l, "history.undo", lowerFirst(OperationTitle(l, t)))
			} else {
				h.Operations[i].Title = i18n.T(l, "history.undo_unknown")
			}
		} else {
			h.Operations[i].Title = OperationTitle(l, op)
		}
		h.Operations[i].Change = &ch
		if op.ErrorMsg != nil {
			h.Operations[i].Error = op.ErrorMsg.Text(l)
		}
	}
	return h, nil
}

func changeOf(op journal.Operation) plan.ChangeView {
	var ch plan.ChangeView
	if m := swap.ManagedOf(journal.State{Managed: op.ManagedBefore}); m != nil {
		ch.SwapBefore = &m.SizeBytes
	}
	if m := swap.ManagedOf(journal.State{Managed: op.ManagedAfter}); m != nil {
		ch.SwapAfter = &m.SizeBytes
	}
	if p := profile.ProfileOf(journal.State{Profile: op.ProfileBefore}); p != nil {
		ch.ProfileBefore = p.Name
	}
	if p := profile.ProfileOf(journal.State{Profile: op.ProfileAfter}); p != nil {
		ch.ProfileAfter = p.Name
	}
	return ch
}

// OperationTitle — название операции для человека: «Запас 16 GB из SSD +
// режим «Сжатие — быстрое»».
func OperationTitle(l i18n.Lang, op journal.Operation) string {
	ch := changeOf(op)
	swapPart := func() string {
		switch {
		case ch.SwapAfter == nil && ch.SwapBefore == nil:
			return ""
		case ch.SwapAfter == nil:
			return i18n.T(l, "history.swap_removed")
		case ch.SwapBefore == nil:
			return i18n.T(l, "history.swap_added", gbText(*ch.SwapAfter))
		case *ch.SwapBefore != *ch.SwapAfter:
			return i18n.T(l, "history.swap_resized", gbText(*ch.SwapBefore), gbText(*ch.SwapAfter))
		}
		return ""
	}
	profilePart := func() string {
		switch {
		case ch.ProfileAfter == "" && ch.ProfileBefore != "":
			return i18n.T(l, "history.mode_reset")
		case ch.ProfileAfter != "":
			return i18n.T(l, "history.mode", profile.FriendlyName(l, ch.ProfileAfter))
		}
		return ""
	}
	var parts []string
	switch op.Action {
	case swap.ActionApply, swap.ActionResize, swap.ActionRemove:
		parts = append(parts, swapPart())
	case profile.ActionProfileApply, profile.ActionProfileReset:
		parts = append(parts, profilePart())
	case ActionSetup:
		if s := swapPart(); s != "" {
			parts = append(parts, s)
		}
		// Профиль входил в setup — раздел profile переписан.
		if !bytes.Equal(op.ProfileBefore, op.ProfileAfter) {
			parts = append(parts, profilePart())
		}
	}
	title := strings.Join(nonEmpty(parts), " + ")
	if title == "" {
		title = plan.ActionTitle(l, op.Action)
	}
	return upperFirst(title)
}

func nonEmpty(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToLower(string(r[0])) + string(r[1:])
}
