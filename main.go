package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type ProcessGroup struct {
	DisplayName string
	ProcName    string
	AppName     string
	StreamIDs   []string
}

type model struct {
	groups []ProcessGroup
	cursor int
	status string
	err    error
}

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1)

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#04B575"))

	itemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#DDDDDD")).
			PaddingLeft(3)

	statusStyle = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.Color("#AAAAAA"))
)

func fetchProcessGroups() ([]ProcessGroup, error) {
	cmd := exec.Command("pactl", "list", "sink-inputs")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to query sink-inputs: %w", err)
	}

	raw := out.String()
	if strings.TrimSpace(raw) == "" {
		return []ProcessGroup{}, nil
	}

	blocks := strings.Split(raw, "Sink Input #")
	groupMap := make(map[string]*ProcessGroup)
	var orderedKeys []string

	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		id := strings.TrimSpace(lines[0])

		appName := extractProperty(block, "application.name")
		procName := extractProperty(block, "application.process.binary")
		mediaName := extractProperty(block, "media.name")

		// Filter out internal routing / loopbacks
		if strings.Contains(appName, "Combined_Shared_Sink") ||
			strings.Contains(mediaName, "Combined_Shared_Sink") ||
			strings.Contains(mediaName, "pm_route_") ||
			strings.Contains(appName, "Loopback") {
			continue
		}

		appName = strings.Trim(appName, "\"")
		procName = strings.Trim(procName, "\"")
		mediaName = strings.Trim(mediaName, "\"")

		if appName == "" {
			appName = mediaName
		}
		if appName == "" {
			appName = "Unknown Audio Stream"
		}
		if procName == "" {
			procName = "N/A"
		}

		// Use binary name as primary group key; fallback to AppName
		groupKey := procName
		if groupKey == "N/A" || groupKey == "" {
			groupKey = appName
		}

		if _, exists := groupMap[groupKey]; !exists {
			groupMap[groupKey] = &ProcessGroup{
				DisplayName: appName,
				ProcName:    procName,
				AppName:     appName,
				StreamIDs:   []string{},
			}
			orderedKeys = append(orderedKeys, groupKey)
		}
		groupMap[groupKey].StreamIDs = append(groupMap[groupKey].StreamIDs, id)
	}

	var groups []ProcessGroup
	for _, key := range orderedKeys {
		groups = append(groups, *groupMap[key])
	}

	return groups, nil
}

func extractProperty(block, prop string) string {
	re := regexp.MustCompile(fmt.Sprintf(`%s\s*=\s*(.+)`, regexp.QuoteMeta(prop)))
	matches := re.FindStringSubmatch(block)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func getDefaultSink() (string, error) {
	cmd := exec.Command("pactl", "get-default-sink")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func removeSharedSinks() (string, error) {
	defaultSink, _ := getDefaultSink()

	// Move all inputs back to default hardware sink before deleting modules
	cmdInputs := exec.Command("pactl", "list", "short", "sink-inputs")
	var inputsOut bytes.Buffer
	cmdInputs.Stdout = &inputsOut
	if err := cmdInputs.Run(); err == nil && defaultSink != "" {
		for _, line := range strings.Split(inputsOut.String(), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				inputID := fields[0]
				_ = exec.Command("pactl", "move-sink-input", inputID, defaultSink).Run()
			}
		}
	}

	// Unload all existing combined and null sinks
	cmdModules := exec.Command("pactl", "list", "short", "modules")
	var modOut bytes.Buffer
	cmdModules.Stdout = &modOut
	if err := cmdModules.Run(); err != nil {
		return "", fmt.Errorf("failed to list modules: %w", err)
	}

	unloadedCount := 0
	lines := strings.Split(modOut.String(), "\n")
	for _, line := range lines {
		if strings.Contains(line, "module-combine-sink") || strings.Contains(line, "module-null-sink") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				modID := fields[0]
				_ = exec.Command("pactl", "unload-module", modID).Run()
				unloadedCount++
			}
		}
	}

	return fmt.Sprintf("Reset audio: Unloaded %d virtual module(s).", unloadedCount), nil
}

func setupSharedSinkForGroup(group ProcessGroup) (string, error) {
	_, _ = removeSharedSinks()

	realSink, err := getDefaultSink()
	if err != nil {
		return "", fmt.Errorf("could not get default sink: %w", err)
	}

	// 1. Create a virtual null sink
	_ = exec.Command("pactl", "load-module", "module-null-sink", "sink_name=Virtual_Sink", "sink_properties=device.description=Virtual_Stream_Sink").Run()
	
	time.Sleep(50 * time.Millisecond)

	// 2. Create combined sink targeting both real hardware and virtual sink
	combineCmd := exec.Command("pactl", "load-module", "module-combine-sink",
		"sink_name=Combined_Shared_Sink",
		fmt.Sprintf("slaves=%s,Virtual_Sink", realSink),
		"sink_properties=device.description=Combined_Process_Sink",
	)

	time.Sleep(50 * time.Millisecond)

	if err := combineCmd.Run(); err != nil {
		return "", fmt.Errorf("failed to create combined sink: %w", err)
	}

	// 3. Move all streams in the process group to the combined sink
	movedCount := 0
	for _, id := range group.StreamIDs {
		time.Sleep(50 * time.Millisecond)
		moveCmd := exec.Command("pactl", "move-sink-input", id, "Combined_Shared_Sink")
		if err := moveCmd.Run(); err == nil {
			movedCount++
		}
	}

	return fmt.Sprintf("Split %d stream(s) for '%s' between [%s] & [Virtual_Sink]", movedCount, group.DisplayName, realSink), nil
}

func initialModel() model {
	groups, err := fetchProcessGroups()
	return model{
		groups: groups,
		cursor: 0,
		status: "Select a program and press Enter to split all its audio streams.",
		err:    err,
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "j":
			if m.cursor < len(m.groups)-1 {
				m.cursor++
			}

		case "r":
			m.groups, m.err = fetchProcessGroups()
			if m.cursor >= len(m.groups) {
				m.cursor = 0
			}
			m.status = "Refreshed stream list."

		case "u", "c", "d":
			msg, err := removeSharedSinks()
			if err != nil {
				m.status = fmt.Sprintf("Error: %v", err)
			} else {
				m.status = msg
			}
			m.groups, m.err = fetchProcessGroups()
			m.cursor = 0

		case "enter":
			if len(m.groups) == 0 {
				return m, nil
			}
			target := m.groups[m.cursor]
			statusMsg, err := setupSharedSinkForGroup(target)
			if err != nil {
				m.status = fmt.Sprintf("Error: %v", err)
			} else {
				m.status = statusMsg
			}
			m.groups, m.err = fetchProcessGroups()
		}
	}
	return m, nil
}

func (m model) View() string {
	var s strings.Builder

	s.WriteString(titleStyle.Render("Linux Audio Process Selector & Virtual Splitter") + "\n\n")

	if m.err != nil {
		s.WriteString(fmt.Sprintf("Error querying audio subsystem: %v\n", m.err))
		return s.String()
	}

	if len(m.groups) == 0 {
		s.WriteString("No active audio streams found. Start playing sound and press [r] to refresh.\n\n")
	} else {
		s.WriteString("Active Sound Applications:\n\n")
		for i, group := range m.groups {
			streamCount := len(group.StreamIDs)
			streamsSummary := strings.Join(group.StreamIDs, ", ")
			label := fmt.Sprintf("%s (binary: %s) • [%d stream(s): #%s]", group.DisplayName, group.ProcName, streamCount, streamsSummary)

			if m.cursor == i {
				s.WriteString(selectedStyle.Render(" > "+label) + "\n")
			} else {
				s.WriteString(itemStyle.Render(label) + "\n")
			}
		}
		s.WriteString("\n")
	}

	s.WriteString(statusStyle.Render(m.status) + "\n\n")
	s.WriteString("[↑/↓] Navigate • [Enter] Split Group • [u] Unload/Reset Sinks • [r] Refresh • [q] Quit\n")

	return s.String()
}

func main() {
	p := tea.NewProgram(initialModel())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running application: %v\n", err)
		os.Exit(1)
	}
}
