package agent

import (
	"context"
	"slices"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk/options"
)

const modelsServiceName = "llm.models"

func (a *component) models(_ context.Context, q options.Query) (options.Page, error) {
	page := options.Page{Options: []options.Option{}}
	search := strings.ToLower(q.Search)
	for _, selected := range a.config.models {
		if len(q.Values) > 0 {
			if !slices.Contains(q.Values, selected.name) {
				continue
			}
		} else {
			if kind := q.Filter["type"]; kind != "" && kind != selected.modelType {
				continue
			}
			if !strings.Contains(strings.ToLower(selected.name), search) {
				continue
			}
		}
		if len(page.Options) == q.Limit {
			page.More = true
			break
		}
		page.Options = append(page.Options, options.Option{Value: selected.name, Label: selected.name, Description: selected.provider.name + " · " + selected.model})
	}
	return page, nil
}
