package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/grunyas/grunyas/internal/server/proxy"
)

var (
	headerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1).
			Bold(true)

	statStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("205"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginTop(1)
)

// LogMsg is a message type for new log entries.
type LogMsg string

// model represents the state of our TUI application.
type model struct {
	srv      *proxy.Proxy
	viewport viewport.Model
	ready    bool
	logCh    <-chan string // Channel to receive log messages
	width    int
	height   int
	stats    string // Current pool stats string
	content  string // All log content for the viewport
}

// initialModel creates a new model with initial state.
func initialModel(srv *proxy.Proxy, logCh <-chan string) model {
	return model{
		srv:   srv,
		logCh: logCh,
		stats: "Press 'p' to see pool stats.", // Initial stats message
	}
}

// Init initializes the model. It returns a command to start listening for logs.
func (m model) Init() tea.Cmd {
	return tea.Batch(waitForLog(m.logCh), tickCmd())
}

// Update handles messages and updates the model's state.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		cmd  tea.Cmd
		cmds []tea.Cmd
	)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "p", "P":
			// Fetch stats
			s := m.srv.PoolStats()
			m.stats = fmt.Sprintf(
				"Total: %d | Acquired: %d | Idle: %d | Max: %d",
				s.TotalConns, s.AcquiredConns, s.IdleConns, s.MaxConns,
			)
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		headerHeight := lipgloss.Height(m.headerView())
		footerHeight := lipgloss.Height(m.footerView())

		if !m.ready {
			m.viewport = viewport.New(msg.Width, msg.Height-headerHeight-footerHeight)
			m.viewport.YPosition = headerHeight
			m.viewport.SetContent(m.content) // Set initial content if any
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = msg.Height - headerHeight - footerHeight
		}
		m.viewport.GotoBottom() // Keep viewport scrolled to bottom on resize

	case LogMsg:
		// Append new log message to content and update viewport
		m.content += string(msg) + "\n"
		m.viewport.SetContent(m.content)
		m.viewport.GotoBottom()                  // Scroll to bottom to show new log
		cmds = append(cmds, waitForLog(m.logCh)) // Continue listening for logs

	case tickMsg:
		// Update stats periodically (e.g., every second)
		s := m.srv.PoolStats()
		m.stats = fmt.Sprintf(
			"Total: %d | Acquired: %d | Idle: %d | Max: %d",
			s.TotalConns, s.AcquiredConns, s.IdleConns, s.MaxConns,
		)
		cmds = append(cmds, tickCmd()) // Schedule next tick
	}

	// Pass messages to the viewport for scrolling
	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// View renders the TUI.
func (m model) View() string {
	if !m.ready {
		return "Initializing..."
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		m.headerView(),
		m.viewport.View(),
		m.footerView(),
	)
}

// headerView renders the header section.
func (m model) headerView() string {
	title := headerStyle.Render("Grunyas")
	line := strings.Repeat("─", max(0, m.width-lipgloss.Width(title)))
	return lipgloss.JoinHorizontal(lipgloss.Center, title, line)
}

// footerView renders the footer section with stats and help.
func (m model) footerView() string {
	sslMode := "unknown"
	if m.srv != nil {
		sslMode = m.srv.GetConfig().ServerConfig.SSLMode
	}

	statsLine := statStyle.Render(fmt.Sprintf("%s | SSL: %s", m.stats, sslMode))
	helpLine := helpStyle.Render("Press 'q' or 'ctrl+c' to quit. Use arrow keys/mouse to scroll.")
	return lipgloss.JoinVertical(lipgloss.Left, statsLine, helpLine)
}

// waitForLog is a command that waits for a new log message on the channel.
func waitForLog(logCh <-chan string) tea.Cmd {
	return func() tea.Msg {
		logEntry, ok := <-logCh
		if !ok {
			return tea.Quit // Channel closed
		}
		return LogMsg(logEntry)
	}
}

// tickMsg is a message type for periodic updates.
type tickMsg time.Time

// tickCmd is a command that sends a tickMsg after a delay.
func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Start runs the interactive console TUI.
func Start(ctx context.Context, srv *proxy.Proxy, logCh <-chan string) {
	// Wait for server to be ready
	select {
	case <-srv.Ready():
		// Server initialized successfully
	case <-ctx.Done():
		// Application shutting down before server became ready
		return
	}

	p := tea.NewProgram(initialModel(srv, logCh), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v\n", err)
	}
}
