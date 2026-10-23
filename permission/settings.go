package permission

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Settings is the "permissions" block of Claude Code settings files; see
// https://code.claude.com/docs/en/settings-reference#permission-settings.
type Settings struct {
	// Rules holds the rules of each file, in the order loaded.
	Rules []Rules
	// DefaultMode is the defaultMode of the last file that sets one, or
	// empty.
	DefaultMode Mode
	// AdditionalDirectories are those of all files, in order, resolved
	// against each file's project directory unless absolute or "~/".
	AdditionalDirectories []string
	// DisableBypassPermissionsMode is set if any file sets
	// disableBypassPermissionsMode to "disable".
	DisableBypassPermissionsMode bool
}

// Config returns a Config with the settings, for the working directory
// dir.
func (s *Settings) Config(dir string) Config {
	return Config{
		Mode:                         s.DefaultMode,
		Dir:                          dir,
		AdditionalDirectories:        s.AdditionalDirectories,
		Rules:                        s.Rules,
		DisableBypassPermissionsMode: s.DisableBypassPermissionsMode,
	}
}

type settingsFile struct {
	Permissions *struct {
		Allow                        []string `json:"allow"`
		Ask                          []string `json:"ask"`
		Deny                         []string `json:"deny"`
		DefaultMode                  string   `json:"defaultMode"`
		AdditionalDirectories        []string `json:"additionalDirectories"`
		DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
	} `json:"permissions"`
}

// LoadSettings reads the "permissions" block of Claude Code settings
// files. Pass the files in increasing precedence, for example
// ~/.claude/settings.json, .claude/settings.json and
// .claude/settings.local.json: the last defaultMode wins, while rules from
// every file apply, deny first, so a deny in any file blocks. Files that
// do not exist are skipped.
//
// As in Claude Code, "/path" patterns are relative to the settings
// source: ~/.claude for the user settings file ~/.claude/settings.json,
// the working directory for a project's .claude/settings.json and
// .claude/settings.local.json, and the file's directory for any other
// file.
//
// Rules Claude Code would skip, such as MCP rules with a specifier or path
// rules for Write, and settings agentx does not support, such as
// defaultMode "auto", are left out and described in the returned
// warnings. The error reports unreadable files and malformed JSON.
func LoadSettings(paths ...string) (*Settings, []string, error) {
	s := &Settings{}
	var warnings []string
	home, _ := os.UserHomeDir()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		w, err := s.parse(path, data, home)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		warnings = append(warnings, w...)
	}
	return s, warnings, nil
}

// ParseSettings parses the "permissions" block of one settings file whose
// "/path" patterns are relative to the working directory; see
// LoadSettings.
func ParseSettings(data []byte) (*Settings, []string, error) {
	s := &Settings{}
	w, err := s.parse("", data, "")
	if err != nil {
		return nil, nil, err
	}
	return s, w, nil
}

func (s *Settings) parse(path string, data []byte, home string) ([]string, error) {
	var f settingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Permissions == nil {
		return nil, nil
	}
	pm := f.Permissions
	var warnings []string
	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if path != "" {
			msg = path + ": " + msg
		}
		warnings = append(warnings, msg)
	}
	base, project := settingsBase(path, home)
	rs := Rules{Base: base}
	for _, l := range []struct {
		kind listKind
		in   []string
		out  *[]string
	}{{listAllow, pm.Allow, &rs.Allow}, {listAsk, pm.Ask, &rs.Ask}, {listDeny, pm.Deny, &rs.Deny}} {
		for _, text := range l.in {
			if _, err := parseRule(text, l.kind); err != nil {
				warn("skipping %s rule: %v", l.kind, err)
				continue
			}
			*l.out = append(*l.out, text)
		}
	}
	if len(rs.Allow)+len(rs.Ask)+len(rs.Deny) > 0 {
		s.Rules = append(s.Rules, rs)
	}
	if pm.DefaultMode != "" {
		if m, err := ParseMode(pm.DefaultMode); err != nil {
			warn("ignoring defaultMode: %v", err)
		} else {
			s.DefaultMode = m
		}
	}
	for _, d := range pm.AdditionalDirectories {
		if project != "" && !filepath.IsAbs(d) && d != "~" && !strings.HasPrefix(d, "~/") {
			d = joinPath(project, d)
		}
		s.AdditionalDirectories = append(s.AdditionalDirectories, d)
	}
	if pm.DisableBypassPermissionsMode == "disable" {
		s.DisableBypassPermissionsMode = true
	}
	return warnings, nil
}

// settingsBase returns the Rules.Base of a settings file and the project
// directory its relative additional directories resolve against.
func settingsBase(path, home string) (base, project string) {
	if path == "" {
		return "", ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", ""
	}
	dir := filepath.Dir(abs)
	name := filepath.Base(abs)
	if filepath.Base(dir) == ".claude" && (name == "settings.json" || name == "settings.local.json") {
		if home != "" && dir == filepath.Join(home, ".claude") {
			return dir, home
		}
		// Project and local settings anchor at the working directory.
		return "", filepath.Dir(dir)
	}
	return dir, dir
}
