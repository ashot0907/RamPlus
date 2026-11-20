package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/ashot0907/ramplus/internal/core"
	"github.com/ashot0907/ramplus/internal/i18n"
	"github.com/ashot0907/ramplus/internal/journal"
	"github.com/ashot0907/ramplus/internal/plan"
	"github.com/ashot0907/ramplus/internal/render"
)

// errSilent — код выхода 1 без сообщения в stderr: итог уже напечатан в
// stdout (JSON со status и error).
var errSilent = errors.New("silent failure")

// errJSONNeedsYes — в JSON-режиме вопросов нет.
var errJSONNeedsYes = i18n.Errorf("cli.json.needs_yes")

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func addJSONFlag(cmd *cobra.Command, v *bool) {
	cmd.Flags().BoolVar(v, "json", false, T("cli.flag.json"))
}

// showPlan — план текстом или JSON.
func showPlan(cmd *cobra.Command, p *plan.Plan, verbose, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), p)
	}
	return render.Plan(cmd.OutOrStdout(), p, render.Options{Verbose: verbose, Lang: lang})
}

// executeJSON — выполнить без вопросов и напечатать итог JSON
// (core.Result). Неуспех — код выхода 1.
func executeJSON(cmd *cobra.Command, r core.Request, confirm func(*plan.Plan) (bool, error)) error {
	res := newCore().Execute(cmd.Context(), r, journal.Hooks{Confirm: confirm, Origin: cliOrigin(), Lang: lang})
	if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
		return err
	}
	if !res.OK() {
		return errSilent
	}
	return nil
}
