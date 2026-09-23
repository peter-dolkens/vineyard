package managed

import (
	"encoding/json"
	"strings"

	"github.com/peter-dolkens/vineyard/daemon/internal/model"
)

// The initialize response carries more than the model picker: the slash commands this session's
// Claude Code offers (built-in, project and plugin commands, skills) and the signed-in account. The
// chat's "/" menu lists the commands so they can be run from Vineyard, and shows the account.

// parseCommands extracts the slash commands from an initialize response, in Claude Code's order.
func parseCommands(body json.RawMessage) []model.CommandInfo {
	var v struct {
		Commands []struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			ArgumentHint string `json:"argumentHint"`
		} `json:"commands"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	out := make([]model.CommandInfo, 0, len(v.Commands))
	for _, c := range v.Commands {
		name := strings.TrimPrefix(strings.TrimSpace(c.Name), "/")
		if name == "" {
			continue
		}
		out = append(out, model.CommandInfo{Name: name, Description: strings.TrimSpace(c.Description), ArgumentHint: strings.TrimSpace(c.ArgumentHint)})
	}
	return out
}

// parseAccount returns the e-mail of the account the session is signed in as, if reported.
func parseAccount(body json.RawMessage) string {
	return parseAccountInfo(body).Email
}
