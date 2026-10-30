package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxTitleAreas caps how many title areas one project may define.
	MaxTitleAreas = 20
	// MaxTitleAreaNameLen caps an area name, which becomes a title prefix.
	MaxTitleAreaNameLen = 32
	// MaxTitleAreaHintLen caps the description the classifier reads for an area.
	MaxTitleAreaHintLen = 200
	// TitleAreaNone is the classifier's answer for a task that fits no area. It
	// is reserved, so no area can be named after it.
	TitleAreaNone = "NONE"
	// titledSessionNameLimit mirrors the 100-character display-name cap.
	titledSessionNameLimit = 100
)

// TitleArea is one product area of a project, such as one app in a monorepo.
type TitleArea struct {
	// Name is the title prefix, e.g. "S2S".
	Name string `json:"name"`
	// Hint tells the classifier what belongs to the area, e.g. "S2S v1, speech-to-speech".
	Hint string `json:"hint,omitempty"`
}

func validateTitleAreas(areas []TitleArea) error {
	if len(areas) > MaxTitleAreas {
		return fmt.Errorf("titleAreas: at most %d areas", MaxTitleAreas)
	}
	seen := make(map[string]bool, len(areas))
	for i, area := range areas {
		name := area.Name
		switch {
		case strings.TrimSpace(name) == "":
			return fmt.Errorf("titleAreas[%d].name: required", i)
		case strings.TrimSpace(name) != name:
			return fmt.Errorf("titleAreas[%d].name: must not have leading or trailing whitespace", i)
		case utf8.RuneCountInString(name) > MaxTitleAreaNameLen:
			return fmt.Errorf("titleAreas[%d].name: must be %d characters or fewer", i, MaxTitleAreaNameLen)
		case strings.ContainsFunc(name, unicode.IsControl) || strings.Contains(name, ":"):
			return fmt.Errorf("titleAreas[%d].name: must not contain control characters or ':'", i)
		case strings.EqualFold(name, TitleAreaNone):
			return fmt.Errorf("titleAreas[%d].name: %q is reserved", i, name)
		}
		folded := strings.ToLower(name)
		if seen[folded] {
			return fmt.Errorf("titleAreas[%d].name: duplicate area %q", i, name)
		}
		seen[folded] = true
		if utf8.RuneCountInString(area.Hint) > MaxTitleAreaHintLen || strings.ContainsFunc(area.Hint, unicode.IsControl) {
			return fmt.Errorf("titleAreas[%d].hint: must be one line of %d characters or fewer", i, MaxTitleAreaHintLen)
		}
	}
	return nil
}

// MatchTitleArea maps a classifier answer to the configured area it names,
// ignoring letter case and surrounding quotes or punctuation. NONE, an empty
// answer, and anything that names no configured area yield "".
func MatchTitleArea(areas []TitleArea, answer string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(SanitizeControlChars(answer)), "\n")
	line = strings.TrimSpace(line)
	for _, candidate := range []string{line, strings.Trim(line, " \t\r\"'`*.,;:!“”‘’")} {
		if candidate == "" || strings.EqualFold(candidate, TitleAreaNone) {
			return ""
		}
		for _, area := range areas {
			if strings.EqualFold(candidate, area.Name) {
				return area.Name
			}
		}
	}
	return ""
}

// PrefixSessionTitle composes "<area>: <title>". An empty area or title, or a
// title that already starts with "<area>:" in any letter case, comes back
// unchanged. The composed title keeps the display-name cap by shortening the
// title, never the prefix.
func PrefixSessionTitle(area, title string) string {
	if area == "" || title == "" || strings.HasPrefix(strings.ToLower(title), strings.ToLower(area)+":") {
		return title
	}
	prefix := area + ": "
	if room := titledSessionNameLimit - utf8.RuneCountInString(prefix); utf8.RuneCountInString(title) > room {
		title = strings.TrimSpace(string([]rune(title)[:room]))
	}
	return prefix + title
}
