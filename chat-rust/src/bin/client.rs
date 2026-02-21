use std::io;
use std::time::Duration;

use anyhow::Result;
use clap::Parser;
use crossterm::{
    event::{self, Event, KeyCode, KeyEvent, KeyModifiers},
    execute,
    terminal::{disable_raw_mode, enable_raw_mode, EnterAlternateScreen, LeaveAlternateScreen},
};
use ratatui::{
    backend::CrosstermBackend,
    layout::{Alignment, Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear, List, ListItem, Paragraph},
    Frame, Terminal,
};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;
use tokio::sync::mpsc;

use chat::protocol::*;

// ─── CLI ──────────────────────────────────────────────────────────────────────

#[derive(Parser)]
#[command(name = "client", about = "RustChat TUI client")]
struct Args {
    #[arg(long, default_value = "localhost:8080")]
    addr: String,
}

// ─── Screens ─────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, PartialEq)]
enum Screen {
    Login,
    Chat,
    Search,
}

// ─── Simple one-line text input ───────────────────────────────────────────────

#[derive(Default, Clone)]
struct Input {
    value: String,
    cursor: usize,
}

impl Input {
    fn insert(&mut self, ch: char) {
        self.value.insert(self.cursor, ch);
        self.cursor += ch.len_utf8();
    }

    fn delete_back(&mut self) {
        if self.cursor == 0 {
            return;
        }
        // find previous char boundary
        let mut prev = self.cursor - 1;
        while prev > 0 && !self.value.is_char_boundary(prev) {
            prev -= 1;
        }
        self.value.drain(prev..self.cursor);
        self.cursor = prev;
    }

    fn clear(&mut self) {
        self.value.clear();
        self.cursor = 0;
    }

    fn as_str(&self) -> &str {
        &self.value
    }

}

// ─── App state ───────────────────────────────────────────────────────────────

#[derive(Debug, Clone)]
struct ChatLine {
    username: String,
    content: String,
    timestamp: String,
    is_system: bool,
}

struct App {
    screen: Screen,

    // Login fields
    login_field: usize, // 0=username, 1=password
    login_username: Input,
    login_password: Input,
    is_register: bool,
    login_error: String,

    // Chat
    messages: Vec<ChatLine>,
    chat_input: Input,
    online_count: usize,
    scroll: usize,       // how many lines from the bottom we are scrolled
    viewport_height: u16,

    // Search overlay
    search_field: usize, // 0=query, 1=username, 2=from, 3=to
    search_query: Input,
    search_user: Input,
    search_from: Input,
    search_to: Input,
    search_results: Vec<ChatLine>,
    search_scroll: usize,

    // Quit flag
    quit: bool,
}

impl App {
    fn new() -> Self {
        Self {
            screen: Screen::Login,
            login_field: 0,
            login_username: Input::default(),
            login_password: Input::default(),
            is_register: false,
            login_error: String::new(),

            messages: Vec::new(),
            chat_input: Input::default(),
            online_count: 0,
            scroll: 0,
            viewport_height: 20,

            search_field: 0,
            search_query: Input::default(),
            search_user: Input::default(),
            search_from: Input::default(),
            search_to: Input::default(),
            search_results: Vec::new(),
            search_scroll: 0,

            quit: false,
        }
    }

    fn push_message(&mut self, line: ChatLine) {
        self.messages.push(line);
        // If we're at the bottom, stay there
        if self.scroll == 0 {
            // already pinned to bottom
        }
    }

    fn scroll_up(&mut self) {
        let max = self.messages.len().saturating_sub(self.viewport_height as usize);
        self.scroll = (self.scroll + 3).min(max);
    }

    fn scroll_down(&mut self) {
        self.scroll = self.scroll.saturating_sub(3);
    }

    fn search_scroll_up(&mut self) {
        let max = self.search_results.len().saturating_sub(self.viewport_height as usize);
        self.search_scroll = (self.search_scroll + 3).min(max);
    }

    fn search_scroll_down(&mut self) {
        self.search_scroll = self.search_scroll.saturating_sub(3);
    }
}

// ─── Network message types (from server → TUI) ───────────────────────────────

enum NetMsg {
    Packet(Packet),
    Disconnected,
}

// ─── Main ────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();

    // Connect to server
    let stream = TcpStream::connect(&args.addr).await?;
    let (reader, writer) = stream.into_split();

    // Channel: server → UI
    let (net_tx, mut net_rx) = mpsc::channel::<NetMsg>(128);
    // Channel: UI → server writer
    let (write_tx, mut write_rx) = mpsc::channel::<Vec<u8>>(64);

    // Spawn reader task
    tokio::spawn(async move {
        let mut lines = BufReader::new(reader).lines();
        loop {
            match lines.next_line().await {
                Ok(Some(line)) => {
                    if let Ok(pkt) = serde_json::from_str::<Packet>(&line) {
                        if net_tx.send(NetMsg::Packet(pkt)).await.is_err() {
                            break;
                        }
                    }
                }
                _ => {
                    net_tx.send(NetMsg::Disconnected).await.ok();
                    break;
                }
            }
        }
    });

    // Spawn writer task
    tokio::spawn(async move {
        let mut w = writer;
        while let Some(data) = write_rx.recv().await {
            if w.write_all(&data).await.is_err() {
                break;
            }
        }
    });

    // Set up terminal
    enable_raw_mode()?;
    let mut stdout = io::stdout();
    execute!(stdout, EnterAlternateScreen)?;
    let backend = CrosstermBackend::new(stdout);
    let mut terminal = Terminal::new(backend)?;

    let mut app = App::new();
    let result = run_app(&mut terminal, &mut app, &mut net_rx, &write_tx).await;

    // Restore terminal
    disable_raw_mode()?;
    execute!(terminal.backend_mut(), LeaveAlternateScreen)?;
    terminal.show_cursor()?;

    result
}

async fn run_app(
    terminal: &mut Terminal<CrosstermBackend<io::Stdout>>,
    app: &mut App,
    net_rx: &mut mpsc::Receiver<NetMsg>,
    write_tx: &mpsc::Sender<Vec<u8>>,
) -> Result<()> {
    loop {
        // Draw
        let size = terminal.size()?;
        app.viewport_height = size.height.saturating_sub(6);
        terminal.draw(|f| draw(f, app))?;

        // Poll keyboard (non-blocking, 20ms)
        if event::poll(Duration::from_millis(20))? {
            if let Event::Key(key) = event::read()? {
                handle_key(app, key, write_tx).await?;
            }
        }

        // Drain all pending network messages
        while let Ok(msg) = net_rx.try_recv() {
            handle_net(app, msg, write_tx).await?;
        }

        if app.quit {
            break;
        }
    }
    Ok(())
}

// ─── Key handling ─────────────────────────────────────────────────────────────

async fn handle_key(
    app: &mut App,
    key: KeyEvent,
    write_tx: &mpsc::Sender<Vec<u8>>,
) -> Result<()> {
    match app.screen {
        Screen::Login => handle_login_key(app, key, write_tx).await,
        Screen::Chat => handle_chat_key(app, key, write_tx).await,
        Screen::Search => handle_search_key(app, key, write_tx).await,
    }
}

async fn handle_login_key(
    app: &mut App,
    key: KeyEvent,
    write_tx: &mpsc::Sender<Vec<u8>>,
) -> Result<()> {
    match key.code {
        KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.quit = true;
        }
        KeyCode::Char('q') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.quit = true;
        }
        KeyCode::Char('r') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.is_register = !app.is_register;
            app.login_error.clear();
        }
        KeyCode::Tab => {
            app.login_field = if app.login_field == 0 { 1 } else { 0 };
        }
        KeyCode::BackTab => {
            app.login_field = if app.login_field == 1 { 0 } else { 1 };
        }
        KeyCode::Enter => {
            let username = app.login_username.value.trim().to_string();
            let password = app.login_password.value.clone();
            if username.is_empty() || password.is_empty() {
                app.login_error = "Username and password are required".into();
                return Ok(());
            }
            let msg_type = if app.is_register {
                MessageType::Register
            } else {
                MessageType::Login
            };
            let payload = AuthPayload { username, password };
            send_packet(write_tx, msg_type, payload).await?;
        }
        KeyCode::Backspace => {
            if app.login_field == 0 {
                app.login_username.delete_back();
            } else {
                app.login_password.delete_back();
            }
        }
        KeyCode::Char(c) => {
            if app.login_field == 0 {
                app.login_username.insert(c);
            } else {
                app.login_password.insert(c);
            }
        }
        _ => {}
    }
    Ok(())
}

async fn handle_chat_key(
    app: &mut App,
    key: KeyEvent,
    write_tx: &mpsc::Sender<Vec<u8>>,
) -> Result<()> {
    match key.code {
        KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.quit = true;
        }
        KeyCode::Char('q') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.quit = true;
        }
        KeyCode::Char('f') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.screen = Screen::Search;
            app.search_results.clear();
            app.search_scroll = 0;
        }
        KeyCode::PageUp => app.scroll_up(),
        KeyCode::PageDown => app.scroll_down(),
        KeyCode::Enter => {
            let content = app.chat_input.value.trim().to_string();
            if content.is_empty() {
                return Ok(());
            }
            app.chat_input.clear();
            send_packet(write_tx, MessageType::Chat, ChatPayload { content }).await?;
        }
        KeyCode::Backspace => {
            app.chat_input.delete_back();
        }
        KeyCode::Char(c) => {
            app.chat_input.insert(c);
        }
        _ => {}
    }
    Ok(())
}

async fn handle_search_key(
    app: &mut App,
    key: KeyEvent,
    write_tx: &mpsc::Sender<Vec<u8>>,
) -> Result<()> {
    match key.code {
        KeyCode::Esc => {
            app.screen = Screen::Chat;
        }
        KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.quit = true;
        }
        KeyCode::Tab => {
            app.search_field = (app.search_field + 1) % 4;
        }
        KeyCode::BackTab => {
            app.search_field = (app.search_field + 3) % 4;
        }
        KeyCode::PageUp => app.search_scroll_up(),
        KeyCode::PageDown => app.search_scroll_down(),
        KeyCode::Enter => {
            let payload = SearchPayload {
                query: app.search_query.value.trim().to_string(),
                username: app.search_user.value.trim().to_string(),
                from: parse_datetime(app.search_from.as_str()),
                to: parse_datetime(app.search_to.as_str()),
            };
            if payload.query.is_empty()
                && payload.username.is_empty()
                && payload.from.is_none()
                && payload.to.is_none()
            {
                return Ok(());
            }
            send_packet(write_tx, MessageType::Search, payload).await?;
        }
        KeyCode::Backspace => {
            active_search_field(app).delete_back();
        }
        KeyCode::Char(c) => {
            active_search_field(app).insert(c);
        }
        _ => {}
    }
    Ok(())
}

fn active_search_field(app: &mut App) -> &mut Input {
    match app.search_field {
        0 => &mut app.search_query,
        1 => &mut app.search_user,
        2 => &mut app.search_from,
        _ => &mut app.search_to,
    }
}

// ─── Network message handling ─────────────────────────────────────────────────

async fn handle_net(
    app: &mut App,
    msg: NetMsg,
    write_tx: &mpsc::Sender<Vec<u8>>,
) -> Result<()> {
    match msg {
        NetMsg::Disconnected => {
            app.push_message(ChatLine {
                username: String::new(),
                content: "Disconnected from server.".into(),
                timestamp: String::new(),
                is_system: true,
            });
        }
        NetMsg::Packet(pkt) => match pkt.msg_type {
            MessageType::Broadcast => {
                if let Ok(p) = serde_json::from_value::<BroadcastPayload>(pkt.payload) {
                    let ts = p.timestamp.format("%H:%M:%S").to_string();
                    app.push_message(ChatLine {
                        username: p.username,
                        content: p.content,
                        timestamp: ts,
                        is_system: false,
                    });
                }
            }
            MessageType::System => {
                let text = pkt
                    .payload
                    .get("message")
                    .and_then(|v| v.as_str())
                    .unwrap_or("")
                    .to_string();
                app.push_message(ChatLine {
                    username: String::new(),
                    content: text,
                    timestamp: String::new(),
                    is_system: true,
                });
            }
            MessageType::Response => {
                if let Ok(p) = serde_json::from_value::<ResponsePayload>(pkt.payload) {
                    if app.screen == Screen::Login {
                        if p.success {
                            // Switch to chat, request history
                            app.screen = Screen::Chat;
                            app.login_error.clear();
                            send_packet(
                                write_tx,
                                MessageType::History,
                                HistoryPayload { limit: 50 },
                            )
                            .await?;
                            send_packet(write_tx, MessageType::Users, serde_json::json!({}))
                                .await?;
                        } else {
                            app.login_error = p.message;
                        }
                    } else if app.screen == Screen::Search {
                        // Parse search results
                        app.search_results.clear();
                        if let Some(data) = p.data {
                            if let Ok(msgs) =
                                serde_json::from_value::<Vec<StoredMessage>>(data)
                            {
                                for m in msgs {
                                    let ts = m.timestamp.format("%H:%M:%S").to_string();
                                    app.search_results.push(ChatLine {
                                        username: m.username,
                                        content: m.content,
                                        timestamp: ts,
                                        is_system: false,
                                    });
                                }
                            }
                        }
                    } else {
                        // History or users response while in chat
                        if let Some(data) = p.data {
                            // Try to parse as history
                            if let Ok(msgs) =
                                serde_json::from_value::<Vec<StoredMessage>>(data.clone())
                            {
                                // Prepend history messages
                                let mut history: Vec<ChatLine> = msgs
                                    .into_iter()
                                    .map(|m| {
                                        let ts = m.timestamp.format("%H:%M:%S").to_string();
                                        ChatLine {
                                            username: m.username,
                                            content: m.content,
                                            timestamp: ts,
                                            is_system: false,
                                        }
                                    })
                                    .collect();
                                history.extend(app.messages.drain(..));
                                app.messages = history;
                            } else if let Ok(users) =
                                serde_json::from_value::<Vec<UserInfo>>(data)
                            {
                                app.online_count = users.len();
                            }
                        }
                    }
                }
            }
            _ => {}
        },
    }
    Ok(())
}

// ─── Drawing ─────────────────────────────────────────────────────────────────

fn draw(f: &mut Frame, app: &App) {
    match app.screen {
        Screen::Login => draw_login(f, app),
        Screen::Chat => draw_chat(f, app),
        Screen::Search => {
            draw_chat(f, app);
            draw_search_overlay(f, app);
        }
    }
}

fn draw_login(f: &mut Frame, app: &App) {
    let area = f.area();

    let block = Block::default()
        .title(" RustChat ")
        .title_alignment(Alignment::Center)
        .borders(Borders::ALL)
        .border_style(Style::default().fg(Color::Cyan));

    let inner = block.inner(area);
    f.render_widget(block, area);

    let mode = if app.is_register { "Register" } else { "Login" };
    let hint = if app.is_register {
        "Ctrl+R to switch to Login"
    } else {
        "Ctrl+R to switch to Register"
    };

    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(2), // title
            Constraint::Length(3), // username
            Constraint::Length(3), // password
            Constraint::Length(1), // hint
            Constraint::Length(1), // error
            Constraint::Min(0),
        ])
        .split(inner);

    let title = Paragraph::new(format!("── {} ──", mode))
        .alignment(Alignment::Center)
        .style(Style::default().fg(Color::Yellow).add_modifier(Modifier::BOLD));
    f.render_widget(title, chunks[0]);

    let u_style = if app.login_field == 0 {
        Style::default().fg(Color::Yellow)
    } else {
        Style::default().fg(Color::White)
    };
    let username_widget = Paragraph::new(app.login_username.as_str())
        .block(
            Block::default()
                .title(" Username ")
                .borders(Borders::ALL)
                .border_style(u_style),
        )
        .style(Style::default().fg(Color::White));
    f.render_widget(username_widget, chunks[1]);

    let p_style = if app.login_field == 1 {
        Style::default().fg(Color::Yellow)
    } else {
        Style::default().fg(Color::White)
    };
    let masked: String = "*".repeat(app.login_password.value.len());
    let password_widget = Paragraph::new(masked)
        .block(
            Block::default()
                .title(" Password ")
                .borders(Borders::ALL)
                .border_style(p_style),
        )
        .style(Style::default().fg(Color::White));
    f.render_widget(password_widget, chunks[2]);

    let hint_widget = Paragraph::new(format!("{} | Tab to switch fields | Enter to submit", hint))
        .alignment(Alignment::Center)
        .style(Style::default().fg(Color::DarkGray));
    f.render_widget(hint_widget, chunks[3]);

    if !app.login_error.is_empty() {
        let err = Paragraph::new(app.login_error.as_str())
            .alignment(Alignment::Center)
            .style(Style::default().fg(Color::Red));
        f.render_widget(err, chunks[4]);
    }

    // Place cursor
    if app.login_field == 0 {
        f.set_cursor_position((
            chunks[1].x + 1 + app.login_username.cursor as u16,
            chunks[1].y + 1,
        ));
    } else {
        f.set_cursor_position((
            chunks[2].x + 1 + app.login_password.cursor as u16,
            chunks[2].y + 1,
        ));
    }
}

fn draw_chat(f: &mut Frame, app: &App) {
    let area = f.area();

    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(1),  // header
            Constraint::Min(3),     // messages
            Constraint::Length(3),  // input
        ])
        .split(area);

    // Header
    let header = Paragraph::new(format!(
        " RustChat  │  {} online  │  Ctrl+F search  │  PgUp/PgDn scroll  │  Ctrl+Q quit ",
        app.online_count
    ))
    .style(
        Style::default()
            .bg(Color::DarkGray)
            .fg(Color::White)
            .add_modifier(Modifier::BOLD),
    );
    f.render_widget(header, chunks[0]);

    // Messages viewport
    let msg_block = Block::default()
        .borders(Borders::LEFT | Borders::RIGHT | Borders::TOP)
        .border_style(Style::default().fg(Color::DarkGray));
    let msg_inner = msg_block.inner(chunks[1]);
    f.render_widget(msg_block, chunks[1]);

    let height = msg_inner.height as usize;
    let total = app.messages.len();
    let start = if total > height + app.scroll {
        total - height - app.scroll
    } else {
        0
    };
    let visible = &app.messages[start..total.saturating_sub(app.scroll)];

    let items: Vec<ListItem> = visible
        .iter()
        .map(|line| {
            if line.is_system {
                ListItem::new(Line::from(vec![Span::styled(
                    format!("  ◆ {}", line.content),
                    Style::default()
                        .fg(Color::DarkGray)
                        .add_modifier(Modifier::ITALIC),
                )]))
            } else {
                ListItem::new(Line::from(vec![
                    Span::styled(
                        format!("[{}] ", line.timestamp),
                        Style::default().fg(Color::DarkGray),
                    ),
                    Span::styled(
                        format!("{}: ", line.username),
                        Style::default()
                            .fg(Color::Cyan)
                            .add_modifier(Modifier::BOLD),
                    ),
                    Span::raw(line.content.clone()),
                ]))
            }
        })
        .collect();

    let list = List::new(items);
    f.render_widget(list, msg_inner);

    // Input box
    let input_block = Block::default()
        .title(" Message (Enter to send) ")
        .borders(Borders::ALL)
        .border_style(Style::default().fg(Color::Cyan));
    let input_inner = input_block.inner(chunks[2]);
    f.render_widget(input_block, chunks[2]);

    let input_widget =
        Paragraph::new(app.chat_input.as_str()).style(Style::default().fg(Color::White));
    f.render_widget(input_widget, input_inner);

    // Cursor in input
    if app.screen == Screen::Chat {
        f.set_cursor_position((
            input_inner.x + app.chat_input.cursor as u16,
            input_inner.y,
        ));
    }
}

fn draw_search_overlay(f: &mut Frame, app: &App) {
    let area = f.area();

    // Center overlay: 70% wide, 80% tall
    let popup = centered_rect(70, 80, area);

    f.render_widget(Clear, popup);

    let block = Block::default()
        .title(" Search Messages  (Esc to close | Tab to move | Enter to search | PgUp/PgDn scroll) ")
        .borders(Borders::ALL)
        .border_style(Style::default().fg(Color::Yellow));
    let inner = block.inner(popup);
    f.render_widget(block, popup);

    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(3), // query
            Constraint::Length(3), // username
            Constraint::Length(3), // from
            Constraint::Length(3), // to
            Constraint::Min(0),    // results
        ])
        .split(inner);

    let fields = [
        ("Content", &app.search_query, 0),
        ("Username", &app.search_user, 1),
        ("From (YYYY-MM-DD)", &app.search_from, 2),
        ("To (YYYY-MM-DD)", &app.search_to, 3),
    ];

    for (label, input, idx) in &fields {
        let focused = app.search_field == *idx;
        let border_style = if focused {
            Style::default().fg(Color::Yellow)
        } else {
            Style::default().fg(Color::White)
        };
        let widget = Paragraph::new(input.as_str())
            .block(
                Block::default()
                    .title(format!(" {} ", label))
                    .borders(Borders::ALL)
                    .border_style(border_style),
            )
            .style(Style::default().fg(Color::White));
        f.render_widget(widget, chunks[*idx]);
    }

    // Show cursor in active field
    let active_input = match app.search_field {
        0 => &app.search_query,
        1 => &app.search_user,
        2 => &app.search_from,
        _ => &app.search_to,
    };
    f.set_cursor_position((
        chunks[app.search_field].x + 1 + active_input.cursor as u16,
        chunks[app.search_field].y + 1,
    ));

    // Results
    let results_area = chunks[4];
    let height = results_area.height as usize;
    let total = app.search_results.len();
    let start = if total > height + app.search_scroll {
        total - height - app.search_scroll
    } else {
        0
    };
    let end = total.saturating_sub(app.search_scroll);
    let visible = if start < end { &app.search_results[start..end] } else { &[] };

    let items: Vec<ListItem> = visible
        .iter()
        .map(|line| {
            ListItem::new(Line::from(vec![
                Span::styled(
                    format!("[{}] ", line.timestamp),
                    Style::default().fg(Color::DarkGray),
                ),
                Span::styled(
                    format!("{}: ", line.username),
                    Style::default().fg(Color::Green).add_modifier(Modifier::BOLD),
                ),
                Span::raw(line.content.clone()),
            ]))
        })
        .collect();

    if items.is_empty() && app.search_results.is_empty() {
        let hint = Paragraph::new("Enter search criteria above and press Enter")
            .alignment(Alignment::Center)
            .style(Style::default().fg(Color::DarkGray));
        f.render_widget(hint, results_area);
    } else if items.is_empty() {
        let hint = Paragraph::new("No results found")
            .alignment(Alignment::Center)
            .style(Style::default().fg(Color::DarkGray));
        f.render_widget(hint, results_area);
    } else {
        let list = List::new(items);
        f.render_widget(list, results_area);
    }
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

fn centered_rect(percent_x: u16, percent_y: u16, r: Rect) -> Rect {
    let popup_layout = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Percentage((100 - percent_y) / 2),
            Constraint::Percentage(percent_y),
            Constraint::Percentage((100 - percent_y) / 2),
        ])
        .split(r);

    Layout::default()
        .direction(Direction::Horizontal)
        .constraints([
            Constraint::Percentage((100 - percent_x) / 2),
            Constraint::Percentage(percent_x),
            Constraint::Percentage((100 - percent_x) / 2),
        ])
        .split(popup_layout[1])[1]
}

async fn send_packet(
    write_tx: &mpsc::Sender<Vec<u8>>,
    msg_type: MessageType,
    payload: impl serde::Serialize,
) -> Result<()> {
    let pkt = Packet::new(msg_type, payload)?;
    let mut data = serde_json::to_vec(&pkt)?;
    data.push(b'\n');
    write_tx.send(data).await.ok();
    Ok(())
}

fn parse_datetime(s: &str) -> Option<chrono::DateTime<chrono::Utc>> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }
    // Try YYYY-MM-DD format, treating as midnight UTC
    chrono::NaiveDate::parse_from_str(s, "%Y-%m-%d")
        .ok()
        .map(|d| {
            d.and_hms_opt(0, 0, 0)
                .unwrap()
                .and_utc()
        })
        .or_else(|| chrono::DateTime::parse_from_rfc3339(s).ok().map(|dt| dt.with_timezone(&chrono::Utc)))
}
