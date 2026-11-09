package codexprojects

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type Section struct {
	ID   string
	Name string
}

type Placement struct {
	ProjectID       string
	SectionID       string
	SectionPosition int64
	Pinned          bool
}

type Sidebar struct {
	Projects        []Project
	Sections        []Section
	Threads         map[string]Placement
	PinnedThreadIDs []string
	RemotePinTitles map[string]string
}

func Load() ([]Project, error) {
	sidebar, err := LoadSidebar()
	return sidebar.Projects, err
}

func LoadSidebar() (Sidebar, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return Sidebar{}, err
		}
		home = filepath.Join(user, ".codex")
	}
	sidebar, err := readDesktopSidebar(filepath.Join(home, "state_5.sqlite"))
	if err == nil {
		readGlobalSidebar(home, &sidebar)
		return sidebar, nil
	}
	legacy, legacyErr := loadLegacy(home)
	if legacyErr == nil {
		sidebar := Sidebar{Projects: legacy}
		readGlobalSidebar(home, &sidebar)
		return sidebar, nil
	}
	if errors.Is(err, os.ErrNotExist) && errors.Is(legacyErr, os.ErrNotExist) {
		return Sidebar{}, nil
	}
	return Sidebar{}, errors.Join(err, legacyErr)
}

func readDesktopSidebar(path string) (Sidebar, error) {
	if _, err := os.Stat(path); err != nil {
		return Sidebar{}, err
	}
	file := filepath.ToSlash(path)
	if !strings.HasPrefix(file, "/") {
		file = "/" + file
	}
	dsn := url.URL{Scheme: "file", Path: file, RawQuery: "mode=ro&_pragma=busy_timeout%281500%29"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return Sidebar{}, err
	}
	defer func() { _ = db.Close() }()
	var sidebar Sidebar
	if err := readProjects(db, &sidebar); err != nil {
		return Sidebar{}, err
	}
	if err := readSections(db, &sidebar); err != nil {
		return Sidebar{}, err
	}
	if err := readPlacements(db, &sidebar); err != nil {
		return Sidebar{}, err
	}
	return sidebar, nil
}

func readProjects(db *sql.DB, sidebar *Sidebar) error {
	rows, err := db.Query("SELECT p.id, p.name, r.path FROM projects p JOIN project_roots r ON p.id=r.project_id ORDER BY p.position, p.id, r.position")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	indices := map[string]int{}
	for rows.Next() {
		var id, name, path string
		if err := rows.Scan(&id, &name, &path); err != nil {
			return err
		}
		if !filepath.IsAbs(path) {
			continue
		}
		idx, exists := indices[id]
		if !exists {
			idx = len(sidebar.Projects)
			indices[id] = idx
			sidebar.Projects = append(sidebar.Projects, Project{ID: id, Name: name, Path: filepath.Clean(path)})
		}
		sidebar.Projects[idx].Roots = append(sidebar.Projects[idx].Roots, filepath.Clean(path))
	}
	return rows.Err()
}

func readSections(db *sql.DB, sidebar *Sidebar) error {
	rows, err := db.Query("SELECT id, name FROM thread_sections ORDER BY rowid")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var section Section
		if err := rows.Scan(&section.ID, &section.Name); err != nil {
			return err
		}
		sidebar.Sections = append(sidebar.Sections, section)
	}
	return rows.Err()
}

func readPlacements(db *sql.DB, sidebar *Sidebar) error {
	const columns = "SELECT id, COALESCE(project_id,''), COALESCE(thread_section_id,''), COALESCE(is_pinned,0), "
	const from = " FROM threads WHERE archived=0"
	rows, err := db.Query(columns + "COALESCE(section_position,0)" + from)
	if err != nil && strings.Contains(err.Error(), "no such column: section_position") {
		rows, err = db.Query(columns + "0" + from)
	}
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	sidebar.Threads = map[string]Placement{}
	for rows.Next() {
		var id string
		var place Placement
		if err := rows.Scan(&id, &place.ProjectID, &place.SectionID, &place.Pinned, &place.SectionPosition); err != nil {
			return err
		}
		sidebar.Threads[id] = place
	}
	return rows.Err()
}

func readGlobalSidebar(home string, sidebar *Sidebar) {
	data, err := os.ReadFile(filepath.Join(home, ".codex-global-state.json"))
	if err != nil {
		return
	}
	var desktop struct {
		Pinned             []string                     `json:"pinned-thread-ids"`
		Atoms              map[string]json.RawMessage   `json:"electron-persisted-atom-state"`
		MigratedProjects   map[string]map[string]string `json:"app-server-project-id-by-legacy-project-id-by-host"`
		ThreadProjectHosts map[string]string            `json:"thread-project-membership-host-ids"`
		ThreadProjects     map[string]struct {
			Kind string `json:"projectKind"`
			ID   string `json:"projectId"`
		} `json:"thread-project-assignments"`
	}
	if err := json.Unmarshal(data, &desktop); err != nil {
		return
	}
	localProjects := map[string]bool{}
	for _, project := range sidebar.Projects {
		localProjects[project.ID] = project.ID != ""
	}
	migrated := desktop.MigratedProjects["local:"+filepath.Clean(home)]
	for id, project := range desktop.ThreadProjects {
		place, known := sidebar.Threads[id]
		host := desktop.ThreadProjectHosts[id]
		if !known || place.ProjectID != "" || project.Kind != "local" || (host != "" && host != "local") {
			continue
		}
		projectID := project.ID
		if newID := migrated[projectID]; newID != "" {
			projectID = newID
		}
		if localProjects[projectID] {
			place.ProjectID = projectID
			sidebar.Threads[id] = place
		}
	}
	pinned := map[string]bool{}
	for _, id := range desktop.Pinned {
		pinned[id] = id != ""
	}
	for id, place := range sidebar.Threads {
		if place.Pinned {
			pinned[id] = true
		}
	}
	var order []string
	_ = json.Unmarshal(desktop.Atoms["app-server-pinned-thread-order-v1"], &order)
	seen := map[string]bool{}
	for _, id := range append(order, desktop.Pinned...) {
		if pinned[id] && !seen[id] {
			sidebar.PinnedThreadIDs = append(sidebar.PinnedThreadIDs, id)
			seen[id] = true
		}
	}
	sidebar.RemotePinTitles = map[string]string{}
	for key, raw := range desktop.Atoms {
		if !strings.HasPrefix(key, "remote-thread-summaries-v3:") {
			continue
		}
		var summaries []struct {
			ID    string `json:"conversationId"`
			Title string `json:"title"`
		}
		if err := json.Unmarshal(raw, &summaries); err != nil {
			continue
		}
		for _, summary := range summaries {
			if pinned[summary.ID] && summary.Title != "" {
				sidebar.RemotePinTitles[summary.ID] = summary.Title
			}
		}
	}
}
