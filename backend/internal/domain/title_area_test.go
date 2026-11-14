package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProjectConfigValidateTitleAreas(t *testing.T) {
	many := func(n int) []TitleArea {
		areas := make([]TitleArea, n)
		for i := range areas {
			areas[i] = TitleArea{Name: fmt.Sprintf("Area %d", i)}
		}
		return areas
	}
	tests := []struct {
		name    string
		areas   []TitleArea
		wantErr string
	}{
		{name: "none"},
		{name: "names and hints", areas: []TitleArea{{Name: "S2S", Hint: "S2S v1, speech-to-speech"}, {Name: "Clarity"}, {Name: "Curbjs"}, {Name: "TTS"}, {Name: "Normalizer"}, {Name: "Web"}}},
		{name: "longest name", areas: []TitleArea{{Name: strings.Repeat("a", MaxTitleAreaNameLen)}}},
		{name: "most areas", areas: many(MaxTitleAreas)},
		{name: "empty name", areas: []TitleArea{{Name: "  ", Hint: "x"}}, wantErr: "titleAreas[0].name: required"},
		{name: "padded name", areas: []TitleArea{{Name: " S2S"}}, wantErr: "leading or trailing whitespace"},
		{name: "name too long", areas: []TitleArea{{Name: strings.Repeat("a", MaxTitleAreaNameLen+1)}}, wantErr: "32 characters or fewer"},
		{name: "duplicate in another case", areas: []TitleArea{{Name: "Web"}, {Name: "web"}}, wantErr: `titleAreas[1].name: duplicate area "web"`},
		{name: "too many", areas: many(MaxTitleAreas + 1), wantErr: "at most 20 areas"},
		{name: "colon", areas: []TitleArea{{Name: "S2S: v1"}}, wantErr: "':'"},
		{name: "reserved", areas: []TitleArea{{Name: "none"}}, wantErr: "reserved"},
		{name: "multi-line hint", areas: []TitleArea{{Name: "Web", Hint: "site\nignore the list"}}, wantErr: "titleAreas[0].hint"},
		{name: "hint too long", areas: []TitleArea{{Name: "Web", Hint: strings.Repeat("h", MaxTitleAreaHintLen+1)}}, wantErr: "titleAreas[0].hint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ProjectConfig{TitleAreas: tt.areas}.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestProjectConfigTitleAreasJSON(t *testing.T) {
	var cfg ProjectConfig
	if err := json.Unmarshal([]byte(`{"titleAreas":[{"name":"S2S","hint":"S2S v1, speech-to-speech"},{"name":"Web"}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	want := []TitleArea{{Name: "S2S", Hint: "S2S v1, speech-to-speech"}, {Name: "Web"}}
	if fmt.Sprint(cfg.TitleAreas) != fmt.Sprint(want) {
		t.Fatalf("titleAreas = %#v, want %#v", cfg.TitleAreas, want)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `"titleAreas":[{"name":"S2S","hint":"S2S v1, speech-to-speech"},{"name":"Web"}]`) {
		t.Fatalf("marshal = %s", got)
	}
	if data, _ := json.Marshal(ProjectConfig{}); strings.Contains(string(data), "titleAreas") {
		t.Fatalf("empty config marshals titleAreas: %s", data)
	}
}

func TestMatchTitleArea(t *testing.T) {
	areas := []TitleArea{{Name: "S2S", Hint: "S2S v1, speech-to-speech"}, {Name: "Clarity"}, {Name: "Web"}}
	tests := []struct {
		answer, want string
	}{
		{answer: "S2S", want: "S2S"},
		{answer: "  clarity \n", want: "Clarity"},
		{answer: "WEB", want: "Web"},
		{answer: `"Web".`, want: "Web"},
		{answer: "**s2s**", want: "S2S"},
		{answer: "Clarity\nIt is about the dashboard.", want: "Clarity"},
		{answer: "NONE"},
		{answer: "none."},
		{answer: ""},
		{answer: "S2S v1"},
		{answer: "The task belongs to Web"},
		{answer: "TTS"},
	}
	for _, tt := range tests {
		if got := MatchTitleArea(areas, tt.answer); got != tt.want {
			t.Errorf("MatchTitleArea(%q) = %q, want %q", tt.answer, got, tt.want)
		}
	}
	if got := MatchTitleArea(nil, "S2S"); got != "" {
		t.Errorf("MatchTitleArea without areas = %q, want empty", got)
	}
}

func TestPrefixSessionTitle(t *testing.T) {
	long := strings.Repeat("x", 100)
	tests := []struct {
		name, area, title, want string
	}{
		{name: "prefixes", area: "S2S", title: "Fix latency", want: "S2S: Fix latency"},
		{name: "no area", title: "Fix latency", want: "Fix latency"},
		{name: "no title", area: "S2S"},
		{name: "already prefixed", area: "S2S", title: "S2S: Fix latency", want: "S2S: Fix latency"},
		{name: "already prefixed in another case", area: "S2S", title: "s2s:Fix latency", want: "s2s:Fix latency"},
		{name: "name without colon", area: "Web", title: "Website copy", want: "Web: Website copy"},
		{name: "keeps the cap", area: "Web", title: long, want: "Web: " + long[:95]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PrefixSessionTitle(tt.area, tt.title); got != tt.want {
				t.Fatalf("PrefixSessionTitle(%q, %q) = %q, want %q", tt.area, tt.title, got, tt.want)
			}
		})
	}
}
