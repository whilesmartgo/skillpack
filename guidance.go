package skillpack

import "strings"

// Departure is a choice to go against one of a skill's checks, for the items
// named, with the reason. It asks for an exception; the findings still stand.
type Departure struct {
	Check string   `json:"check"`
	Items []string `json:"items"`
	Why   string   `json:"why"`
}

const framing = "The skills below are craft to draw on, not a routine to follow. Their practices are heuristics: " +
	"choose the ones that serve this piece of work, adapt them, and leave out what does not fit.\n\n" +
	"When you go against one of a skill's checks on purpose, record a departure: the check id, the " +
	"items it affects, and why. A departure asks for an exception; it does not remove the finding. " +
	"You do not need a departure for anything you simply did not use."

// GuidanceOptions adjust the prompt Guidance builds.
type GuidanceOptions struct {
	// AlreadyUsed names choices recent work already made, so the next piece
	// does not repeat them.
	AlreadyUsed []string
	// NamesOnly lists skills by name and description without their bodies,
	// for a model that reads them through Tools.
	NamesOnly bool
}

// Guidance builds a prompt that offers skills as direction for one task.
func Guidance(skills []*Skill, task string, options GuidanceOptions) string {
	parts := []string{framing, "## Task\n\n" + strings.TrimSpace(task)}
	for _, skill := range skills {
		section := "## Skill: " + skill.Name() + "\n\n" + skill.Description()
		if !options.NamesOnly {
			section += "\n\n" + strings.TrimSpace(skill.Body)
		}
		if checks := skill.Checks(); len(checks) > 0 {
			lines := make([]string, 0, len(checks))
			for _, check := range checks {
				lines = append(lines, strings.TrimRight("- "+check.ID+": "+check.Description, ": "))
			}
			section += "\n\nChecks for this skill:\n" + strings.Join(lines, "\n")
		}
		parts = append(parts, section)
	}
	used := []string{}
	for _, item := range options.AlreadyUsed {
		if item = strings.TrimSpace(item); item != "" {
			used = append(used, "- "+item)
		}
	}
	if len(used) > 0 {
		parts = append(parts, "## Already used\n\nRecent work already made these choices. Pick something else "+
			"unless the task asks for one of them:\n"+strings.Join(used, "\n"))
	}
	parts = append(parts, "## Departures\n\nIf you depart from a check, list each departure as "+
		`{"check": "<check id>", "items": ["<what it affects>"], "why": "<reason>"}.`)
	return strings.Join(parts, "\n\n")
}
