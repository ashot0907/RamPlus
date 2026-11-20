package core

import (
	"context"

	"github.com/ashot0907/ramplus/internal/profile"
)

// ProfilesSchemaVersion — версия JSON-схемы ProfileCatalog.
const ProfilesSchemaVersion = 1

// ProfileEntry — профиль каталога (для веба: карточки режимов и таблица
// параметров ядра).
type ProfileEntry struct {
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Friendly    string            `json:"friendly"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	StatusText  string            `json:"status_text"`
	Evidence    string            `json:"evidence"`
	Params      map[string]string `json:"params"`
	Recommended bool              `json:"recommended"`
	Active      bool              `json:"active"`
}

// ParamEntry — параметр, который могут менять профили (--set).
type ParamEntry struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Path    string   `json:"path"`
	Group   string   `json:"group"` // zswap | sysctl
	Type    string   `json:"type"`  // bool | int | word
	Min     *int     `json:"min,omitempty"`
	Max     *int     `json:"max,omitempty"`
	Words   []string `json:"words,omitempty"`
	Current *string  `json:"current"` // null — нет в этом ядре
}

// ProfileCatalog — GET /v1/profiles.
type ProfileCatalog struct {
	SchemaVersion int            `json:"schema_version"`
	Profiles      []ProfileEntry `json:"profiles"`
	Params        []ParamEntry   `json:"params"`
	Active        string         `json:"active"` // применённый профиль ("" — нет)
	ZswapPresent  bool           `json:"zswap_present"`
}

// Profiles — каталог профилей, параметры и их текущие значения. Только
// чтение.
func (c *Core) Profiles(ctx context.Context) ProfileCatalog {
	pf := profile.Gather(ctx, c.Sys, nil)
	cat := ProfileCatalog{SchemaVersion: ProfilesSchemaVersion, Profiles: []ProfileEntry{}, Params: []ParamEntry{}, ZswapPresent: pf.ZswapPresent}
	if pf.Profile != nil {
		cat.Active = pf.Profile.Name
	}
	for _, p := range profile.All() {
		cat.Profiles = append(cat.Profiles, ProfileEntry{Name: p.Name, Title: p.Title, Friendly: p.Friendly, Description: p.Description,
			Status: p.Status, StatusText: profile.StatusText(p.Status), Evidence: p.Evidence, Params: p.Params,
			Recommended: p.Name == RecommendedProfile, Active: p.Name == cat.Active})
	}
	types := map[profile.Kind]string{profile.KindBool: "bool", profile.KindInt: "int", profile.KindWord: "word"}
	for _, pr := range profile.Params {
		e := ParamEntry{Key: pr.Key, Title: pr.Title, Path: pr.Path, Group: pr.Group, Type: types[pr.Kind], Words: pr.Words}
		if pr.Kind == profile.KindInt {
			lo, hi := pr.Min, pr.Max
			e.Min, e.Max = &lo, &hi
		}
		if v, ok := pf.Values[pr.Key]; ok {
			e.Current = &v
		}
		cat.Params = append(cat.Params, e)
	}
	return cat
}
