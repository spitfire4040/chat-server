// GoChat TUI client.
//
// Screens
// -------
//   stateLogin  – centered login / register form
//   stateChat   – full-screen chat with scrollable message viewport
//   stateSearch – Ctrl+F overlay: 4 search fields + scrollable results
//
// Concurrency
// -----------
//   A single goroutine reads newline-delimited JSON from the TCP connection
//   and forwards raw bytes to the pkts channel.  The Bubbletea event loop
//   consumes one packet at a time via waitForPkt (a tea.Cmd), immediately
//   queuing the next read after each packet is processed.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"chat/internal/protocol"
)

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

var (
	purple = lipgloss.Color("99")
	cyan   = lipgloss.Color("86")
	green  = lipgloss.Color("82")
	red    = lipgloss.Color("196")
	yellow = lipgloss.Color("220")
	gray   = lipgloss.Color("241")
	white  = lipgloss.Color("255")
	orange = lipgloss.Color("214")
	blue   = lipgloss.Color("75")
	teal   = lipgloss.Color("30")

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Background(purple).
			Foreground(white).
			Padding(0, 1)

	searchHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Background(teal).
				Foreground(white).
				Padding(0, 1)

	footerBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.NormalBorder(), true, false, false, false).
				BorderForeground(gray).
				Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(purple).
			Padding(0, 2)

	labelStyle = lipgloss.NewStyle().
			Foreground(gray).
			Width(10)

	focusedLabelStyle = lipgloss.NewStyle().
				Foreground(cyan).
				Width(10)

	hintStyle = lipgloss.NewStyle().
			Foreground(gray).
			Italic(true)

	successStyle = lipgloss.NewStyle().Foreground(green)
	errorStyle   = lipgloss.NewStyle().Foreground(red)
	sysStyle     = lipgloss.NewStyle().Foreground(yellow).Italic(true)
	tsStyle      = lipgloss.NewStyle().Foreground(gray)
	myNameStyle  = lipgloss.NewStyle().Bold(true).Foreground(orange)
	peerStyle    = lipgloss.NewStyle().Bold(true).Foreground(blue)
	divStyle     = lipgloss.NewStyle().Foreground(gray)
)

// ---------------------------------------------------------------------------
// Bubbletea message types
// ---------------------------------------------------------------------------

type serverPktMsg []byte    // a raw packet line arrived from the server
type disconnectedMsg struct{} // server closed the connection

// ---------------------------------------------------------------------------
// Application state
// ---------------------------------------------------------------------------

type appState int

const (
	stateLogin  appState = iota
	stateChat
	stateSearch
)

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type model struct {
	conn net.Conn
	pkts chan []byte // goroutine → bubbletea bridge

	state appState
	me    string // authenticated username

	// Login / register
	loginIsReg  bool
	loginFocus  int
	loginFields [2]textinput.Model // [0]=username  [1]=password
	statusMsg   string

	// Chat
	ready       bool
	viewport    viewport.Model
	chatInput   textinput.Model
	chatLines   []string // rendered lines shown in the viewport
	onlineCount int

	// Search overlay
	searchFocus   int
	searchFields  [4]textinput.Model // content / username / from / to
	searchResults []protocol.StoredMessage
	searchStatus  string
	waitSearch    bool // true while waiting for the server's search response
	waitHistory   bool // true while waiting for the initial history response

	width, height int
}

func newModel(conn net.Conn, pkts chan []byte) model {
	// --- login fields ---
	uf := textinput.New()
	uf.Placeholder = "username"
	uf.Focus()
	uf.CharLimit = 32
	uf.Width = 32

	pf := textinput.New()
	pf.Placeholder = "password"
	pf.EchoMode = textinput.EchoPassword
	pf.EchoCharacter = '•'
	pf.CharLimit = 64
	pf.Width = 32

	// --- chat input ---
	ci := textinput.New()
	ci.Placeholder = "Type a message…"
	ci.CharLimit = 500

	// --- search fields ---
	labels := []string{"content substring", "username (exact)", "YYYY-MM-DD", "YYYY-MM-DD"}
	var sf [4]textinput.Model
	for i := range sf {
		f := textinput.New()
		f.Placeholder = labels[i]
		f.CharLimit = 64
		f.Width = 36
		sf[i] = f
	}

	return model{
		conn:         conn,
		pkts:         pkts,
		state:        stateLogin,
		loginFields:  [2]textinput.Model{uf, pf},
		chatInput:    ci,
		searchFields: sf,
	}
}

// ---------------------------------------------------------------------------
// Tea interface – Init
// ---------------------------------------------------------------------------

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, waitForPkt(m.pkts))
}

// ---------------------------------------------------------------------------
// Tea interface – Update
// ---------------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.viewport = viewport.New(msg.Width, m.vpHeight())
			m.ready = true
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = m.vpHeight()
		}
		m.chatInput.Width = msg.Width - 4
		return m, nil

	case serverPktMsg:
		m = m.handleServerPkt([]byte(msg))
		return m, waitForPkt(m.pkts)

	case disconnectedMsg:
		m.statusMsg = "disconnected from server"
		return m, tea.Quit

	case tea.KeyMsg:
		switch m.state {
		case stateLogin:
			return m.handleLoginKey(msg)
		case stateChat:
			return m.handleChatKey(msg)
		case stateSearch:
			return m.handleSearchKey(msg)
		}
	}
	return m, nil
}

// vpHeight returns the number of lines available for the chat viewport.
func (m model) vpHeight() int {
	// header (1) + footer border (1) + footer input (1) = 3 lines reserved
	h := m.height - 3
	if h < 1 {
		h = 1
	}
	return h
}

// ---------------------------------------------------------------------------
// Key handlers
// ---------------------------------------------------------------------------

func (m model) handleLoginKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyTab, tea.KeyShiftTab:
		if msg.Type == tea.KeyTab {
			m.loginFocus = (m.loginFocus + 1) % 2
		} else {
			m.loginFocus = (m.loginFocus + 1) % 2 // only 2 fields, same either way
		}
		for i := range m.loginFields {
			if i == m.loginFocus {
				m.loginFields[i].Focus()
			} else {
				m.loginFields[i].Blur()
			}
		}
		return m, textinput.Blink

	case tea.KeyCtrlR:
		m.loginIsReg = !m.loginIsReg
		m.statusMsg = ""
		return m, nil

	case tea.KeyEnter:
		user := strings.TrimSpace(m.loginFields[0].Value())
		pass := m.loginFields[1].Value()
		if user == "" || pass == "" {
			m.statusMsg = "username and password are required"
			return m, nil
		}
		if m.loginIsReg {
			sendPkt(m.conn, protocol.TypeRegister, protocol.AuthPayload{Username: user, Password: pass})
		} else {
			sendPkt(m.conn, protocol.TypeLogin, protocol.AuthPayload{Username: user, Password: pass})
		}
		m.statusMsg = "Authenticating…"
		return m, nil
	}

	// Forward keystroke to the focused login field.
	var cmd tea.Cmd
	m.loginFields[m.loginFocus], cmd = m.loginFields[m.loginFocus].Update(msg)
	return m, cmd
}

func (m model) handleChatKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlQ:
		sendPkt(m.conn, protocol.TypeQuit, map[string]string{})
		return m, tea.Quit

	case tea.KeyCtrlF:
		// Open search overlay.
		m.state = stateSearch
		m.searchStatus = ""
		m.searchResults = nil
		m.searchFocus = 0
		m.searchFields[0].Focus()
		for i := 1; i < 4; i++ {
			m.searchFields[i].Blur()
		}
		return m, textinput.Blink

	case tea.KeyEnter:
		content := strings.TrimSpace(m.chatInput.Value())
		if content != "" {
			sendPkt(m.conn, protocol.TypeChat, protocol.ChatPayload{Content: content})
			m.chatInput.Reset()
		}
		return m, nil

	case tea.KeyPgUp:
		m.viewport.HalfViewUp()
		return m, nil

	case tea.KeyPgDown:
		m.viewport.HalfViewDown()
		return m, nil
	}

	var cmd tea.Cmd
	m.chatInput, cmd = m.chatInput.Update(msg)
	return m, cmd
}

func (m model) handleSearchKey(msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		sendPkt(m.conn, protocol.TypeQuit, map[string]string{})
		return m, tea.Quit

	case tea.KeyEsc:
		// Close search, return to chat.
		m.state = stateChat
		m.chatInput.Focus()
		return m, textinput.Blink

	case tea.KeyTab:
		m.searchFocus = (m.searchFocus + 1) % 4
		for i := range m.searchFields {
			if i == m.searchFocus {
				m.searchFields[i].Focus()
			} else {
				m.searchFields[i].Blur()
			}
		}
		return m, textinput.Blink

	case tea.KeyShiftTab:
		m.searchFocus = (m.searchFocus + 3) % 4
		for i := range m.searchFields {
			if i == m.searchFocus {
				m.searchFields[i].Focus()
			} else {
				m.searchFields[i].Blur()
			}
		}
		return m, textinput.Blink

	case tea.KeyEnter:
		return m.executeSearch()
	}

	var cmd tea.Cmd
	m.searchFields[m.searchFocus], cmd = m.searchFields[m.searchFocus].Update(msg)
	return m, cmd
}

// executeSearch validates the date fields, builds the payload, and sends it.
func (m model) executeSearch() (model, tea.Cmd) {
	p := protocol.SearchPayload{
		Query:    strings.TrimSpace(m.searchFields[0].Value()),
		Username: strings.TrimSpace(m.searchFields[1].Value()),
	}

	fromStr := strings.TrimSpace(m.searchFields[2].Value())
	if fromStr != "" {
		t, err := time.ParseInLocation("2006-01-02", fromStr, time.Local)
		if err != nil {
			m.searchStatus = errorStyle.Render("From: invalid date — use YYYY-MM-DD")
			return m, nil
		}
		p.From = &t
	}

	toStr := strings.TrimSpace(m.searchFields[3].Value())
	if toStr != "" {
		t, err := time.ParseInLocation("2006-01-02", toStr, time.Local)
		if err != nil {
			m.searchStatus = errorStyle.Render("To: invalid date — use YYYY-MM-DD")
			return m, nil
		}
		// Include the entire "to" day.
		endOfDay := t.Add(24*time.Hour - time.Second)
		p.To = &endOfDay
	}

	if p.Query == "" && p.Username == "" && p.From == nil && p.To == nil {
		m.searchStatus = errorStyle.Render("enter at least one search criterion")
		return m, nil
	}

	sendPkt(m.conn, protocol.TypeSearch, p)
	m.searchStatus = hintStyle.Render("Searching…")
	m.searchResults = nil
	m.waitSearch = true
	return m, nil
}

// ---------------------------------------------------------------------------
// Server packet handler
// ---------------------------------------------------------------------------

func (m model) handleServerPkt(data []byte) model {
	var pkt protocol.Packet
	if err := json.Unmarshal(data, &pkt); err != nil {
		return m
	}

	switch pkt.Type {

	case protocol.TypeBroadcast:
		var b protocol.BroadcastPayload
		if err := json.Unmarshal(pkt.Payload, &b); err != nil {
			return m
		}
		ts := tsStyle.Render("[" + b.Timestamp.Local().Format("15:04:05") + "]")
		var name string
		if b.Username == m.me {
			name = myNameStyle.Render(b.Username)
		} else {
			name = peerStyle.Render(b.Username)
		}
		m.appendChat(ts + " " + name + ": " + b.Content)

	case protocol.TypeSystem:
		var sys map[string]string
		if err := json.Unmarshal(pkt.Payload, &sys); err != nil {
			return m
		}
		msg := sys["message"]
		m.appendChat(sysStyle.Render("⚡ " + msg))
		// Track rough online count from join/leave announcements.
		if strings.HasSuffix(msg, "joined the chat") {
			m.onlineCount++
		} else if strings.HasSuffix(msg, "left the chat") && m.onlineCount > 0 {
			m.onlineCount--
		}

	case protocol.TypeResponse:
		var r protocol.ResponsePayload
		if err := json.Unmarshal(pkt.Payload, &r); err != nil {
			return m
		}

		// ---- auth success ----
		if r.Success && (strings.Contains(r.Message, "logged in as") ||
			strings.Contains(r.Message, "registered and logged in as")) {
			m.me = extractQuoted(r.Message)
			m.state = stateChat
			m.chatInput.Focus()
			// Request recent history right away.
			sendPkt(m.conn, protocol.TypeHistory, protocol.HistoryPayload{Limit: 50})
			m.waitHistory = true
			m.onlineCount = 1
			return m
		}

		// ---- search response ----
		if m.waitSearch {
			m.waitSearch = false
			if r.Success {
				var msgs []protocol.StoredMessage
				if err := json.Unmarshal(r.Data, &msgs); err == nil {
					m.searchResults = msgs
					m.searchStatus = successStyle.Render(r.Message)
				} else {
					m.searchStatus = successStyle.Render("0 results")
				}
			} else {
				m.searchStatus = errorStyle.Render(r.Message)
				m.searchResults = nil
			}
			return m
		}

		// ---- history response ----
		if m.waitHistory && r.Success {
			m.waitHistory = false
			var msgs []protocol.StoredMessage
			if err := json.Unmarshal(r.Data, &msgs); err == nil && len(msgs) > 0 {
				lines := make([]string, 0, len(msgs))
				for _, msg := range msgs {
					ts := tsStyle.Render("[" + msg.Timestamp.Local().Format("15:04:05") + "]")
					var name string
					if msg.Username == m.me {
						name = myNameStyle.Render(msg.Username)
					} else {
						name = peerStyle.Render(msg.Username)
					}
					lines = append(lines, ts+" "+name+": "+msg.Content)
				}
				// Prepend history before any live messages that may have arrived.
				m.chatLines = append(lines, m.chatLines...)
				m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
				m.viewport.GotoBottom()
			}
			return m
		}

		// ---- auth failure or other server error ----
		if !r.Success {
			if m.state == stateLogin {
				m.statusMsg = r.Message
			} else {
				m.appendChat(errorStyle.Render("⚠ " + r.Message))
			}
		}
	}
	return m
}

// appendChat adds a rendered line and scrolls the viewport to the bottom.
func (m *model) appendChat(line string) {
	m.chatLines = append(m.chatLines, line)
	m.viewport.SetContent(strings.Join(m.chatLines, "\n"))
	m.viewport.GotoBottom()
}

// ---------------------------------------------------------------------------
// Tea interface – View
// ---------------------------------------------------------------------------

func (m model) View() string {
	switch m.state {
	case stateLogin:
		return m.viewLogin()
	case stateChat:
		return m.viewChat()
	case stateSearch:
		return m.viewSearch()
	}
	return ""
}

func (m model) viewLogin() string {
	if m.width == 0 {
		return "\n  Connecting to server…"
	}

	mode := "Login"
	other := "Register"
	if m.loginIsReg {
		mode, other = "Register", "Login"
	}

	title := titleStyle.Render("  GoChat Terminal  ")

	renderField := func(label string, f textinput.Model, focused bool) string {
		var lbl string
		if focused {
			lbl = focusedLabelStyle.Render(label)
		} else {
			lbl = labelStyle.Render(label)
		}
		return lbl + "  " + f.View()
	}

	form := lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		renderField("Username", m.loginFields[0], m.loginFocus == 0),
		renderField("Password", m.loginFields[1], m.loginFocus == 1),
		"",
		hintStyle.Render(fmt.Sprintf("Tab: switch field   Enter: %s   Ctrl+R: switch to %s", mode, other)),
		hintStyle.Render("Ctrl+C: quit"),
		"",
		m.renderStatus(),
	)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, form)
}

func (m model) viewChat() string {
	if !m.ready {
		return "\n  Connecting…"
	}

	hdr := headerStyle.
		Width(m.width).
		Render(fmt.Sprintf(" GoChat  ·  %s  ·  %d online  ·  Ctrl+F: Search  PgUp/Dn: Scroll  Ctrl+C: Quit",
			m.me, m.onlineCount))

	footer := footerBorderStyle.
		Width(m.width - 2).
		Render(m.chatInput.View())

	return lipgloss.JoinVertical(lipgloss.Left, hdr, m.viewport.View(), footer)
}

func (m model) viewSearch() string {
	if m.width == 0 {
		return "\n  Loading…"
	}

	hdr := searchHeaderStyle.
		Width(m.width).
		Render(" Search History  ·  Esc: return to chat  Ctrl+C: quit")

	fieldLabels := []string{"Content", "User", "From", "To"}
	fieldHints := []string{"", "", "(YYYY-MM-DD, optional)", "(YYYY-MM-DD, optional)"}

	var fieldLines []string
	for i, f := range m.searchFields {
		var lbl string
		if m.searchFocus == i {
			lbl = focusedLabelStyle.Render(fieldLabels[i])
		} else {
			lbl = labelStyle.Render(fieldLabels[i])
		}
		hint := ""
		if fieldHints[i] != "" {
			hint = "  " + hintStyle.Render(fieldHints[i])
		}
		fieldLines = append(fieldLines, "  "+lbl+"  "+f.View()+hint)
	}

	keyHint := hintStyle.Render("  Tab: next field   Enter: search   Esc: close")
	div := divStyle.Render(strings.Repeat("─", m.width))

	// Results section.
	var resultLines []string
	if m.searchStatus != "" {
		resultLines = append(resultLines, "  "+m.searchStatus)
	}
	if len(m.searchResults) > 0 {
		resultLines = append(resultLines, "")
		for _, r := range m.searchResults {
			ts := tsStyle.Render("[" + r.Timestamp.Local().Format("2006-01-02 15:04:05") + "]")
			var name string
			if r.Username == m.me {
				name = myNameStyle.Render(r.Username)
			} else {
				name = peerStyle.Render(r.Username)
			}
			resultLines = append(resultLines, "  "+ts+" "+name+": "+r.Content)
		}
	} else if m.searchStatus != "" && !m.waitSearch {
		resultLines = append(resultLines, hintStyle.Render("  (no messages match)"))
	}

	parts := []string{hdr, ""}
	parts = append(parts, fieldLines...)
	parts = append(parts, "", keyHint, div)
	parts = append(parts, resultLines...)

	return strings.Join(parts, "\n")
}

// renderStatus renders the login status line with appropriate colour.
func (m model) renderStatus() string {
	if m.statusMsg == "" {
		return ""
	}
	if strings.Contains(m.statusMsg, "Authenticating") {
		return hintStyle.Render(m.statusMsg)
	}
	return errorStyle.Render(m.statusMsg)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitForPkt returns a tea.Cmd that blocks until the next packet arrives on ch.
// When ch is closed (server disconnected), it returns disconnectedMsg.
func waitForPkt(ch <-chan []byte) tea.Cmd {
	return func() tea.Msg {
		data, ok := <-ch
		if !ok {
			return disconnectedMsg{}
		}
		return serverPktMsg(data)
	}
}

// sendPkt serialises payload into a Packet and writes it as a newline-
// terminated JSON line to conn.
func sendPkt(conn net.Conn, t protocol.MessageType, payload any) {
	pkt, err := protocol.NewPacket(t, payload)
	if err != nil {
		return
	}
	data, err := pkt.Encode()
	if err != nil {
		return
	}
	conn.Write(append(data, '\n'))
}

// extractQuoted returns the first double-quoted string in s.
func extractQuoted(s string) string {
	start := strings.Index(s, `"`)
	if start == -1 {
		return ""
	}
	end := strings.Index(s[start+1:], `"`)
	if end == -1 {
		return ""
	}
	return s[start+1 : start+1+end]
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	addr := flag.String("addr", "localhost:8080", "server address")
	flag.Parse()

	conn, err := net.Dial("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// pkts bridges the TCP reader goroutine and the Bubbletea event loop.
	pkts := make(chan []byte, 64)

	// Reader goroutine: TCP → pkts channel.
	go func() {
		defer close(pkts)
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := make([]byte, len(scanner.Bytes()))
			copy(line, scanner.Bytes())
			pkts <- line
		}
	}()

	p := tea.NewProgram(
		newModel(conn, pkts),
		tea.WithAltScreen(),       // use the alternate screen buffer
		tea.WithMouseCellMotion(), // enable mouse wheel scrolling
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
