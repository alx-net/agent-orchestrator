package cli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestParseTitleAreas(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    []titleArea
		wantErr bool
	}{
		{name: "none"},
		{name: "name and hint", values: []string{"S2S=S2S v1, speech-to-speech"}, want: []titleArea{{Name: "S2S", Hint: "S2S v1, speech-to-speech"}}},
		{name: "name only", values: []string{"Web"}, want: []titleArea{{Name: "Web"}}},
		{name: "trimmed", values: []string{" TTS = text to speech "}, want: []titleArea{{Name: "TTS", Hint: "text to speech"}}},
		{name: "empty hint", values: []string{"Clarity="}, want: []titleArea{{Name: "Clarity"}}},
		{name: "hint keeps later equals", values: []string{"Curbjs=a=b"}, want: []titleArea{{Name: "Curbjs", Hint: "a=b"}}},
		{name: "repeatable", values: []string{"S2S", "Web"}, want: []titleArea{{Name: "S2S"}, {Name: "Web"}}},
		{name: "missing name", values: []string{"=hint"}, wantErr: true},
		{name: "blank", values: []string{"  "}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTitleAreas(tt.values)
			if tt.wantErr {
				if err == nil || ExitCode(err) != 2 {
					t.Fatalf("parseTitleAreas(%q) = %#v, %v; want a usage error", tt.values, got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseTitleAreas(%q) = %#v, %v; want %#v", tt.values, got, err, tt.want)
			}
		})
	}
}

func TestProjectSetConfig_TitleAreaFlags(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := projectServer(t, http.StatusOK, `{"status":"ok","project":{"id":"ka"}}`)
	writeRunFileFor(t, cfg, srv)

	_, errOut, err := executeCLI(t, Deps{
		ProcessAlive: func(int) bool { return true },
	}, "project", "set-config", "ka", "--title-area", "S2S=S2S v1, speech-to-speech", "--title-area", "Web")
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if capture.method != http.MethodPut || capture.path != "/api/v1/projects/ka/config" {
		t.Fatalf("request = %s %s, want PUT /api/v1/projects/ka/config", capture.method, capture.path)
	}
	var got setConfigRequest
	if err := json.Unmarshal(capture.body, &got); err != nil {
		t.Fatalf("decode request body: %v\nbody=%s", err, capture.body)
	}
	want := []titleArea{{Name: "S2S", Hint: "S2S v1, speech-to-speech"}, {Name: "Web"}}
	if !reflect.DeepEqual(got.Config.TitleAreas, want) {
		t.Fatalf("title areas = %#v, want %#v", got.Config.TitleAreas, want)
	}
}

func TestProjectSetConfig_TitleAreaJSON(t *testing.T) {
	cfg := setConfigEnv(t)
	srv, capture := projectServer(t, http.StatusOK, `{"status":"ok","project":{"id":"ka"}}`)
	writeRunFileFor(t, cfg, srv)

	_, errOut, err := executeCLI(t, Deps{
		ProcessAlive: func(int) bool { return true },
	}, "project", "set-config", "ka", "--config-json", `{"sessionPrefix":"ka","titleAreas":[{"name":"TTS","hint":"text to speech"}]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
	}
	if !strings.Contains(string(capture.body), `"titleAreas":[{"name":"TTS","hint":"text to speech"}]`) {
		t.Fatalf("request body = %s, want title areas preserved", capture.body)
	}
}

func TestProjectSetConfig_TitleAreaRejectsMissingName(t *testing.T) {
	setConfigEnv(t)
	_, _, err := executeCLI(t, Deps{}, "project", "set-config", "ka", "--title-area", "=speech")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v (exit %d), want usage error", err, ExitCode(err))
	}
}
