package codexprojects

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDesktopSidebar(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state_5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`
		CREATE TABLE projects(id TEXT, name TEXT, position INTEGER);
		CREATE TABLE project_roots(project_id TEXT, path TEXT, position INTEGER);
		CREATE TABLE thread_sections(id TEXT, name TEXT);
		CREATE TABLE threads(id TEXT, project_id TEXT, thread_section_id TEXT, is_pinned INTEGER, archived INTEGER, section_position INTEGER);
		INSERT INTO projects VALUES ('p','Registered project',0);
		INSERT INTO thread_sections VALUES ('s','My section');
		INSERT INTO threads VALUES ('t','p','s',1,0,10), ('old','p',NULL,0,1,NULL);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO project_roots VALUES ('p',?,0)", root); err != nil {
		t.Fatal(err)
	}
	sidebar, err := readDesktopSidebar(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sidebar.Projects) != 1 || sidebar.Projects[0].Name != "Registered project" || sidebar.Projects[0].Path != root {
		t.Fatalf("wrong projects: %#v", sidebar.Projects)
	}
	if len(sidebar.Sections) != 1 || sidebar.Sections[0].Name != "My section" {
		t.Fatalf("wrong sections: %#v", sidebar.Sections)
	}
	if len(sidebar.Threads) != 1 || sidebar.Threads["t"] != (Placement{ProjectID: "p", SectionID: "s", SectionPosition: 10, Pinned: true}) {
		t.Fatalf("wrong placement: %#v", sidebar.Threads)
	}
}

func TestReadGlobalSidebarFillsMissingLocalProjectAssignments(t *testing.T) {
	home := t.TempDir()
	sidebar := Sidebar{
		Projects: []Project{{ID: "current"}, {ID: "other"}},
		Threads: map[string]Placement{
			"legacy":   {SectionID: "later"},
			"explicit": {ProjectID: "other"},
			"remote":   {},
			"chatgpt":  {},
		},
	}
	state := map[string]any{
		"app-server-project-id-by-legacy-project-id-by-host": map[string]map[string]string{
			"local:" + home: {"previous": "current"},
		},
		"thread-project-membership-host-ids": map[string]string{"remote": "remote-host"},
		"thread-project-assignments": map[string]map[string]string{
			"legacy":   {"projectKind": "local", "projectId": "previous"},
			"explicit": {"projectKind": "local", "projectId": "previous"},
			"remote":   {"projectKind": "local", "projectId": "previous"},
			"chatgpt":  {"projectKind": "chatgpt", "projectId": "previous"},
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, ".codex-global-state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	readGlobalSidebar(home, &sidebar)
	if got := sidebar.Threads["legacy"]; got != (Placement{ProjectID: "current", SectionID: "later"}) {
		t.Fatalf("legacy placement = %#v", got)
	}
	for id, want := range map[string]string{"explicit": "other", "remote": "", "chatgpt": ""} {
		if got := sidebar.Threads[id].ProjectID; got != want {
			t.Errorf("project for %s = %q, want %q", id, got, want)
		}
	}
}
