package skill_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nostosx/agentx/skill"
)

func TestMetadataValidate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		metadata      skill.Metadata
		valid, strict bool
	}{
		{"valid", skill.Metadata{Name: "pdf-tools-2", Description: "Read PDFs"}, true, true},
		{"name maximum", skill.Metadata{Name: strings.Repeat("a", 64), Description: "Description"}, true, true},
		{"unicode description maximum", skill.Metadata{Name: "skill", Description: strings.Repeat("文", 1024)}, true, true},
		{"missing name", skill.Metadata{Description: "Description"}, false, false},
		// Claude Code accepts names the Agent Skills specification does not.
		{"long name", skill.Metadata{Name: strings.Repeat("a", 65), Description: "Description"}, true, false},
		{"uppercase", skill.Metadata{Name: "Skill", Description: "Description"}, true, false},
		{"double hyphen", skill.Metadata{Name: "my--skill", Description: "Description"}, true, false},
		{"nested name", skill.Metadata{Name: "apps/web:deploy", Description: "Description"}, true, false},
		{"words", skill.Metadata{Name: "release notes", Description: "Description"}, true, false},
		{"unicode name", skill.Metadata{Name: "résumé", Description: "Description"}, true, false},
		{"long description", skill.Metadata{Name: "skill", Description: strings.Repeat("文", 1025)}, true, false},
		{"long compatibility", skill.Metadata{Name: "skill", Description: "Description", Compatibility: strings.Repeat("x", 501)}, true, false},
		// Characters Claude Code refuses.
		{"angle bracket", skill.Metadata{Name: "a<b", Description: "Description"}, false, false},
		{"zero-width space", skill.Metadata{Name: "a\u200bb", Description: "Description"}, false, false},
		{"bidi control", skill.Metadata{Name: "a\u202eb", Description: "Description"}, false, false},
		{"control", skill.Metadata{Name: "a\x01b", Description: "Description"}, false, false},
		{"private use", skill.Metadata{Name: "a\ue000b", Description: "Description"}, false, false},
		{"unassigned", skill.Metadata{Name: "a\U000e0fffb", Description: "Description"}, false, false},
		{"no-break space", skill.Metadata{Name: "a\u00a0b", Description: "Description"}, false, false},
		{"double space", skill.Metadata{Name: "a  b", Description: "Description"}, false, false},
		{"leading space", skill.Metadata{Name: " ab", Description: "Description"}, false, false},
		{"reserved", skill.Metadata{Name: "anthropic-skills", Description: "Description"}, false, true},
		{"reserved prefix", skill.Metadata{Name: "anthropic-skills:deploy", Description: "Description"}, false, false},
		{"blank description", skill.Metadata{Name: "skill", Description: " \n "}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.metadata.Validate(); (err == nil) != tt.valid {
				t.Errorf("Validate() = %v, valid=%v", err, tt.valid)
			}
			if err := tt.metadata.ValidateStrict(); (err == nil) != tt.strict {
				t.Errorf("ValidateStrict() = %v, valid=%v", err, tt.strict)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	s, err := skill.New("pdf-tools", "Read PDFs", " \n# Instructions\n ", "read_file", "bash")
	if err != nil {
		t.Fatal(err)
	}
	if s.Metadata().Name != "pdf-tools" || !reflect.DeepEqual(s.Metadata().AllowedTools, []string{"read_file", "bash"}) || s.Dir() != "" {
		t.Errorf("New() metadata = %+v, dir=%q", s.Metadata(), s.Dir())
	}
	if content, err := s.Content(t.Context()); err != nil || content != "# Instructions" {
		t.Errorf("Content() = %q, %v", content, err)
	}
	if _, err := skill.New("in<valid>", "Description", "body"); err == nil {
		t.Error("New accepted invalid metadata")
	}
	d, err := skill.Define(skill.Metadata{Name: "Notes", Arguments: []string{"topic"}}, "\n\nWrite notes about $topic.\nMore.")
	if err != nil || d.Metadata().Description != "Write notes about $topic." {
		t.Errorf("Define() = %+v, %v", d, err)
	}
	if _, err := skill.Define(skill.Metadata{Name: "empty"}, " "); err == nil {
		t.Error("Define accepted a skill without description or content")
	}
}
