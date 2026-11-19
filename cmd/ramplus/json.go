package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/render"
)

// errSilent — код выхода 1 без сообщения в stderr: итог уже напечатан в
// stdout (JSON со status и error).
var errSilent = errors.New("silent failure")

// errJSONNeedsYes — в JSON-режиме вопросов нет.
var errJSONNeedsYes = errors.New("--json при выполнении требует --yes (подтверждения в JSON-режиме нет); план: --dry-run --json")

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func addJSONFlag(cmd *cobra.Command, v *bool) {
	cmd.Flags().BoolVar(v, "json", false, "вывод в JSON (та же схема, что у API демона)")
}

// showPlan — план текстом или JSON.
func showPlan(cmd *cobra.Command, p *plan.Plan, verbose, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), p)
	}
	return render.Plan(cmd.OutOrStdout(), p, render.Options{Verbose: verbose})
}

// executeJSON — выполнить без вопросов и напечатать итог JSON
// (core.Result). Неуспех — код выхода 1.
func executeJSON(cmd *cobra.Command, r core.Request, confirm func(*plan.Plan) (bool, error)) error {
	res := newCore().Execute(cmd.Context(), r, journal.Hooks{Confirm: confirm})
	if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
		return err
	}
	if !res.OK() {
		return errSilent
	}
	return nil
}
