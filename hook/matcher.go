package hook

import (
	"regexp"
	"slices"
	"strings"

	"github.com/nostosx/agentx/internal/toolnames"
)

// matcher selects the events a hook runs for, following Claude Code's
// matcher rules. A nil matcher matches everything.
type matcher struct {
	names []string       // exact names, for a matcher of plain names
	re    *regexp.Regexp // unanchored pattern, for any other matcher
}

// compileMatcher parses a matcher. "", "*" and blank match everything. A
// matcher made only of letters, digits, '_', '-', spaces, ',' and '|' is a
// list of exact names separated by '|' or ','. Any other matcher is a
// regular expression (RE2 syntax) that may match anywhere in the value.
func compileMatcher(s string) (*matcher, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "*" {
		return nil, nil
	}
	if strings.IndexFunc(s, func(r rune) bool { return !plainMatcherRune(r) }) < 0 {
		var names []string
		for name := range strings.FieldsFuncSeq(s, func(r rune) bool { return r == '|' || r == ',' }) {
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return nil, nil
		}
		return &matcher{names: names}, nil
	}
	re, err := regexp.Compile(s)
	if err != nil {
		return nil, err
	}
	return &matcher{re: re}, nil
}

func plainMatcherRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '_' || r == '-' || r == ' ' || r == ',' || r == '|'
}

// match reports whether any of values matches.
func (m *matcher) match(values ...string) bool {
	if m == nil {
		return true
	}
	for _, v := range values {
		if m.re != nil {
			if m.re.MatchString(v) {
				return true
			}
			continue
		}
		if slices.Contains(m.names, v) {
			return true
		}
	}
	return false
}

// toolAliases returns the Claude Code names of an agentx built-in tool, so
// matchers written for Claude Code, such as "Edit|Write", select the agentx
// tools; see internal/toolnames.
func toolAliases(name string) []string { return toolnames.Claude(name) }

// notificationType returns Claude Code's notification_type for the Reason
// of a Notification event.
func notificationType(reason string) string {
	if reason == "approval" {
		return "permission_prompt"
	}
	return reason
}

// matchValues returns the values a matcher is tested against for ev: the
// tool name and its Claude Code names for tool events, Event.Reason and
// the Claude Code notification type for Notification, and Event.Reason
// otherwise.
func matchValues(ev *Event) []string {
	switch ev.Type {
	case PreToolUse, PostToolUse, PostToolUseFailure:
		name := ev.ToolName()
		return append([]string{name}, toolAliases(name)...)
	case Notification:
		if t := notificationType(ev.Reason); t != ev.Reason {
			return []string{ev.Reason, t}
		}
	}
	return []string{ev.Reason}
}
