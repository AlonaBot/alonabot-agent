package agent

import (
	"context"
	"slices"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk"
)

const providersServiceName = "llm.providers"

func (a *component) providers(_ context.Context, q sdk.OptionsQuery) (sdk.OptionsPage, error) {
	page := sdk.OptionsPage{Options: []sdk.Option{}}
	search := strings.ToLower(q.Search)
	for _, selected := range a.config.providers {
		if len(q.Values) > 0 {
			if !slices.Contains(q.Values, selected.name) {
				continue
			}
		} else if !strings.Contains(strings.ToLower(selected.name), search) {
			continue
		}
		if len(page.Options) == q.Limit {
			page.More = true
			break
		}
		page.Options = append(page.Options, sdk.Option{Value: selected.name, Label: selected.name, Description: string(selected.protocol) + " " + selected.model})
	}
	return page, nil
}
