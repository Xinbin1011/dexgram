package main

import (
	"fmt"
	"sort"
	"strings"

	"dexgram/internal/codex"
	"dexgram/internal/codexprojects"
	"dexgram/internal/state"
)

type sessionSection struct {
	ID       string
	Name     string
	Projects []sessionProjectGroup
	Chats    []sessionChat
}

const pinnedChatsSectionID = "codex-pins"

func (s sessionSection) showsChatList() bool {
	return s.ID == pinnedChatsSectionID || strings.HasPrefix(s.ID, "desktop:")
}

type sessionChat struct {
	ThreadIndex int
	ProjectName string
	RemoteTitle string
}

func buildSessionCatalog(threads []codex.Thread, sidebar codexprojects.Sidebar, mappings []state.Conversation, filter string) []sessionSection {
	sections := make([]sessionSection, 0, len(sidebar.Sections)+2)
	sectionIndex := map[string]int{}
	for _, s := range sidebar.Sections {
		sectionIndex[s.ID] = len(sections)
		sections = append(sections, sessionSection{ID: "desktop:" + s.ID, Name: s.Name})
	}
	projectsSection := len(sections)
	sections = append(sections, sessionSection{ID: "projects", Name: "Projects"})
	otherSection := len(sections)
	sections = append(sections, sessionSection{ID: "other", Name: "Chats without a project"})

	byID := map[string]codexprojects.Project{}
	byRoot := map[string]codexprojects.Project{}
	for _, project := range sidebar.Projects {
		id := project.ID
		if id == "" {
			id = normalizeSessionPath(project.Path)
		}
		project.ID = id
		byID[id] = project
		roots := project.Roots
		if len(roots) == 0 {
			roots = []string{project.Path}
		}
		for _, root := range roots {
			key := normalizeSessionPath(root)
			if current, ok := byRoot[key]; ok && current.ID != id {
				byRoot[key] = codexprojects.Project{}
			} else {
				byRoot[key] = project
			}
		}
		if filter == "" {
			ensureSessionProject(&sections[projectsSection], project)
		}
	}

	hints := map[string]codexprojects.Project{}
	for _, conv := range mappings {
		project := byRoot[normalizeSessionPath(conv.CWD)]
		if !conv.Projectless && project.ID != "" && strings.EqualFold(conv.ProjectName, project.Name) {
			hints[conv.CodexThreadID] = project
		}
	}
	chatIndices := map[string]int{}
	chatProjects := make([]codexprojects.Project, len(threads))
	for i, thread := range threads {
		place, known := sidebar.Threads[thread.ID]
		project := byID[place.ProjectID]
		if project.ID == "" && place.ProjectID == "" {
			project = hints[thread.ID]
			if project.ID == "" && !known {
				project = byRoot[normalizeSessionPath(thread.Cwd)]
			}
		}
		chatIndices[thread.ID], chatProjects[i] = i, project
		index := projectsSection
		if project.ID == "" {
			index = otherSection
		}
		addSessionChat(&sections[index], project, i)
		if custom, ok := sectionIndex[place.SectionID]; ok {
			sections[custom].Chats = append(sections[custom].Chats, sessionChat{ThreadIndex: i, ProjectName: project.Name})
		}
	}
	for _, index := range sectionIndex {
		chats := sections[index].Chats
		sort.SliceStable(chats, func(i, j int) bool {
			return sidebar.Threads[threads[chats[i].ThreadIndex].ID].SectionPosition <
				sidebar.Threads[threads[chats[j].ThreadIndex].ID].SectionPosition
		})
	}
	pinned := sessionSection{ID: pinnedChatsSectionID, Name: "Pinned chats"}
	added := map[string]bool{}
	addLocalPin := func(i int) {
		id := threads[i].ID
		if added[id] {
			return
		}
		pinned.Chats = append(pinned.Chats, sessionChat{ThreadIndex: i, ProjectName: chatProjects[i].Name})
		added[id] = true
	}
	for _, id := range sidebar.PinnedThreadIDs {
		if i, ok := chatIndices[id]; ok {
			addLocalPin(i)
		}
	}
	for i, thread := range threads {
		if sidebar.Threads[thread.ID].Pinned {
			addLocalPin(i)
		}
	}
	for _, id := range sidebar.PinnedThreadIDs {
		title := strings.TrimSpace(sidebar.RemotePinTitles[id])
		if !added[id] && title != "" && strings.Contains(strings.ToLower(title), strings.ToLower(filter)) {
			pinned.Chats = append(pinned.Chats, sessionChat{RemoteTitle: title})
			added[id] = true
		}
	}
	if len(pinned.Chats) > 0 {
		sections = append([]sessionSection{pinned}, sections...)
	}
	return sections
}

func addSessionChat(section *sessionSection, project codexprojects.Project, index int) {
	group := ensureSessionProject(section, project)
	group.ThreadIDs = append(group.ThreadIDs, index)
	group.ThreadCount++
}

func ensureSessionProject(section *sessionSection, project codexprojects.Project) *sessionProjectGroup {
	for i := range section.Projects {
		if section.Projects[i].ID == project.ID {
			return &section.Projects[i]
		}
	}
	name := project.Name
	if project.ID == "" {
		name = "Chats without a project"
	}
	section.Projects = append(section.Projects, sessionProjectGroup{ID: project.ID, Name: name, CWD: project.Path})
	return &section.Projects[len(section.Projects)-1]
}

func (a *app) loadSessionCatalog(browser *sessionBrowser) error {
	sidebar, err := codexprojects.LoadSidebar()
	if err != nil {
		return fmt.Errorf("read Codex Desktop projects: %w", err)
	}
	mappings, err := a.store.ListConversations()
	if err != nil {
		return err
	}
	var own []state.Conversation
	for _, conv := range mappings {
		if conv.ChatID == browser.chatID {
			own = append(own, conv)
		}
	}
	if sidebar.RemotePinTitles == nil {
		sidebar.RemotePinTitles = map[string]string{}
	}
	for _, id := range sidebar.PinnedThreadIDs {
		if sidebar.RemotePinTitles[id] != "" {
			continue
		}
		title, err := a.store.GetSetting("remote-pin-title:" + id)
		if err != nil {
			return err
		}
		if title != "" {
			sidebar.RemotePinTitles[id] = title
		}
	}
	browser.sections = buildSessionCatalog(browser.threads, sidebar, own, browser.query)
	browser.desktopPins = map[string]bool{}
	for id, place := range sidebar.Threads {
		if place.Pinned {
			browser.desktopPins[id] = true
		}
	}
	for _, id := range sidebar.PinnedThreadIDs {
		browser.desktopPins[id] = true
	}
	browser.pins, err = a.store.WorkspacePins(browser.chatID)
	return err
}

func (b *sessionBrowser) isPinned(key string) bool {
	if pinned, ok := b.pins[key]; ok {
		return pinned
	}
	id, isThread := strings.CutPrefix(key, "chat:")
	return isThread && b.desktopPins[id]
}

func (b *sessionBrowser) orderedIndices(indices []int, key func(int) string) []int {
	order := append([]int(nil), indices...)
	sort.SliceStable(order, func(i, j int) bool {
		return b.isPinned(key(order[i])) && !b.isPinned(key(order[j]))
	})
	return order
}

func indicesOf(n int) []int {
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	return indices
}
