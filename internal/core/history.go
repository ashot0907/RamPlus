package core

import "github.com/ashot0907/ramplus/internal/journal"

// History — операции из журнала (ID, время, команда, статус, можно ли
// откатить). Только чтение, root не нужен; нет журнала — пустая история.
func (c *Core) History() (journal.History, error) {
	st, err := journal.LoadState(c.Sys)
	if err != nil {
		return journal.HistoryOf(journal.State{}), err
	}
	return journal.HistoryOf(st), nil
}
