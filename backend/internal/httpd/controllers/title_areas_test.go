package controllers_test

import (
	"net/http"
	"reflect"
	"testing"
)

// Title areas round-trip through the project config endpoint, and invalid
// ones are refused when they are set rather than at the next spawn.
func TestProjectsAPI_TitleAreasConfig(t *testing.T) {
	srv := newTestServer(t)
	repo := gitRepo(t, "title-areas")
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/projects", `{"path":`+quote(repo)+`,"projectId":"ka"}`)
	if status != http.StatusCreated {
		t.Fatalf("seed create = %d, want 201; body=%s", status, body)
	}

	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/ka/config",
		`{"config":{"titleAreas":[{"name":"S2S","hint":"S2S v1, speech-to-speech"},{"name":"Web"}]}}`)
	if status != http.StatusOK {
		t.Fatalf("PUT config = %d, want 200; body=%s", status, body)
	}
	type area struct {
		Name string `json:"name"`
		Hint string `json:"hint"`
	}
	want := []area{{Name: "S2S", Hint: "S2S v1, speech-to-speech"}, {Name: "Web"}}
	var updated struct {
		Project struct {
			Config struct {
				TitleAreas []area `json:"titleAreas"`
			} `json:"config"`
		} `json:"project"`
	}
	mustJSON(t, body, &updated)
	if !reflect.DeepEqual(updated.Project.Config.TitleAreas, want) {
		t.Fatalf("PUT titleAreas = %#v, want %#v; body=%s", updated.Project.Config.TitleAreas, want, body)
	}

	body, status, _ = doRequest(t, srv, "GET", "/api/v1/projects/ka", "")
	if status != http.StatusOK {
		t.Fatalf("GET project = %d, want 200; body=%s", status, body)
	}
	mustJSON(t, body, &updated)
	if !reflect.DeepEqual(updated.Project.Config.TitleAreas, want) {
		t.Fatalf("GET titleAreas = %#v, want %#v; body=%s", updated.Project.Config.TitleAreas, want, body)
	}

	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/ka/config", `{"config":{"titleAreas":[{"name":"Web"},{"name":"web"}]}}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_PROJECT_CONFIG")
}

func TestSessionsAPI_GetExposesTitleArea(t *testing.T) {
	svc := newFakeSessionService()
	titled := svc.sessions["ao-1"]
	titled.ID, titled.TitleArea, titled.DisplayName = "ao-2", "S2S", "S2S: Cut latency"
	svc.sessions[titled.ID] = titled
	srv := newSessionTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/sessions?project=ao", "")
	if status != http.StatusOK {
		t.Fatalf("GET sessions = %d, want 200; body=%s", status, body)
	}
	var list struct {
		Sessions []map[string]any `json:"sessions"`
	}
	mustJSON(t, body, &list)
	areas := map[any]any{}
	for _, session := range list.Sessions {
		areas[session["id"]] = session["titleArea"] // nil when omitted
	}
	if len(areas) != 2 || areas["ao-2"] != "S2S" || areas["ao-1"] != nil {
		t.Fatalf("list titleAreas = %v, want ao-2 -> S2S and ao-1 without the field; body=%s", areas, body)
	}
}
