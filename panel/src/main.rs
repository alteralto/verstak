// Пульт верстака: the tickets, what the agent is doing in its container right now, what came out
// (report, patch, steps, where it tried to go), and the buttons around it: a new ticket, stop,
// the isolation probe, take a finished branch. It only reads the tickets' folders and runs
// verstak.exe, docker and git; the agent never runs here.
#![windows_subsystem = "windows"]

use std::collections::{BTreeMap, HashSet};
use std::fs;
use std::io::{Read, Seek, SeekFrom};
use std::os::windows::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use eframe::egui::{self, Color32, RichText};
use serde_json::Value;

const NO_WINDOW: u32 = 0x0800_0000; // CREATE_NO_WINDOW: no console flashes
const DOCKER_DESKTOP: &str = r"C:\Program Files\Docker\Docker\Docker Desktop.exe";

fn main() -> eframe::Result {
    let options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_title("Верстак — пульт")
            .with_inner_size([1180.0, 760.0])
            .with_min_inner_size([760.0, 480.0]),
        ..Default::default()
    };
    eframe::run_native("verstak-panel", options, Box::new(|cc| Ok(Box::new(Panel::new(cc)))))
}

/// A ticket's folder as the list shows it.
#[derive(Clone, Default)]
struct Ticket {
    id: String,
    dir: PathBuf,
    title: String,
    status: String, // the report's, or "running" / "waiting" before there is one
    rounds: u64,
    took_s: u64,
    tokens: (u64, u64, u64), // in, cached, out
    branch: String,
    err: String,
}

/// What a background thread sees every 2 s.
#[derive(Clone, Default)]
struct Scan {
    tickets: Vec<Ticket>,
    docker: bool,
    running: HashSet<String>, // container names
    checked: bool,
}

struct Shared {
    scan: Scan,
    busy: Option<String>,
    note: String,
    probe: Option<String>, // the last isolation probe's output
}

#[derive(PartialEq, Clone, Copy)]
enum Tab {
    Progress,
    Report,
    Changes,
    Steps,
    Network,
    Ticket,
}

struct Panel {
    root: PathBuf,
    work: PathBuf,
    targets: Vec<String>,
    shared: Arc<Mutex<Shared>>,
    selected: Option<String>,
    tab: Tab,
    view: View,
    view_read: Instant,
    form_open: bool,
    form_target: String,
    form_title: String,
    form_body: String,
    show_probe: bool,
}

/// The selected ticket's files, read once a second.
#[derive(Default)]
struct View {
    id: String,
    events: Vec<(Kind, String)>,
    report: String,
    changes: String,
    steps: Vec<Step>,
    network: Vec<(String, u64, u64)>, // host, tunnels, refusals
    ticket: String,
}

impl Panel {
    fn new(cc: &eframe::CreationContext<'_>) -> Self {
        setup_fonts(&cc.egui_ctx);
        let root = find_root();
        let (work, targets) = read_config(&root.join("verstak.json"));
        let shared = Arc::new(Mutex::new(Shared { scan: Scan::default(), busy: None, note: String::new(), probe: None }));
        {
            let (shared, ctx, work) = (shared.clone(), cc.egui_ctx.clone(), work.clone());
            thread::spawn(move || {
                loop {
                    let scan = scan(&work);
                    shared.lock().unwrap().scan = scan;
                    ctx.request_repaint();
                    thread::sleep(Duration::from_secs(2));
                }
            });
        }
        let form_target = targets.first().cloned().unwrap_or_default();
        Panel {
            root,
            work,
            targets,
            shared,
            selected: None,
            tab: Tab::Progress,
            view: View::default(),
            view_read: Instant::now() - Duration::from_secs(10),
            form_open: false,
            form_target,
            form_title: String::new(),
            form_body: String::new(),
            show_probe: false,
        }
    }

    /// A slow action off the UI thread, one at a time; its line goes to the status bar.
    fn run(&self, ctx: &egui::Context, what: &str, job: impl FnOnce() -> String + Send + 'static) {
        let mut sh = self.shared.lock().unwrap();
        if sh.busy.is_some() {
            return;
        }
        sh.busy = Some(what.to_string());
        drop(sh);
        let (shared, ctx) = (self.shared.clone(), ctx.clone());
        thread::spawn(move || {
            let note = job();
            let mut sh = shared.lock().unwrap();
            sh.busy = None;
            sh.note = note;
            ctx.request_repaint();
        });
    }

    fn read_view(&mut self, t: &Ticket) {
        if self.view.id == t.id && self.view_read.elapsed() < Duration::from_secs(1) {
            return;
        }
        self.view_read = Instant::now();
        let out = t.dir.join("out");
        let log = tail(&t.dir.join("container.log"), 512 * 1024);
        let mut v = View { id: t.id.clone(), ..Default::default() };
        v.events = log.lines().filter_map(event_line).collect();
        v.steps = log.lines().filter_map(step_line).collect();
        if v.steps.is_empty() {
            v.steps = tail(&out.join("trace.jsonl"), 256 * 1024).lines().filter_map(step_line).collect();
        }
        v.report = fs::read_to_string(out.join("REPORT.md")).unwrap_or_default();
        v.changes = fs::read_to_string(out.join("changes.diff")).unwrap_or_default();
        if v.changes.is_empty() && !t.branch.is_empty() {
            v.changes = git_out(&self.work.join("inbox.git"), &["log", "-p", "--stat", "-1", &t.branch]);
        }
        v.network = network(&tail(&t.dir.join("egress.jsonl"), 256 * 1024));
        v.ticket = fs::read_to_string(out.join("TICKET.md")).unwrap_or_else(|_| {
            fs::read_to_string(t.dir.join("in").join("job.json")).unwrap_or_default()
        });
        self.view = v;
    }

    fn submit(&mut self, ctx: &egui::Context) {
        let title = self.form_title.trim().to_string();
        if title.is_empty() {
            return;
        }
        let dir = self.work.join("tickets");
        let stamp = now_stamp();
        let file = dir.join(format!("{stamp}.md"));
        let text = format!("# {title}\n\n{}\n", self.form_body.trim());
        let (exe, cfg, target, root) =
            (self.root.join("bin").join("verstak.exe"), self.root.join("verstak.json"), self.form_target.clone(), self.root.clone());
        self.form_title.clear();
        self.form_body.clear();
        self.form_open = false;
        self.run(ctx, "Отдаю тикет в работу", move || {
            if fs::create_dir_all(&dir).and_then(|_| fs::write(&file, text)).is_err() {
                return format!("не записать {}", file.display());
            }
            let log = fs::File::create(dir.join(format!("{stamp}.log")));
            let (out, err) = match log.and_then(|f| Ok((f.try_clone()?, f))) {
                Ok((a, b)) => (Stdio::from(a), Stdio::from(b)),
                Err(_) => (Stdio::null(), Stdio::null()),
            };
            match Command::new(&exe)
                .arg("-config")
                .arg(&cfg)
                .arg("-target")
                .arg(&target)
                .arg(&file)
                .current_dir(&root)
                .stdout(out)
                .stderr(err)
                .creation_flags(NO_WINDOW)
                .spawn()
            {
                Ok(_) => "тикет в работе: контейнер поднимется через несколько секунд".into(),
                Err(e) => format!("не запустить {}: {e}", exe.display()),
            }
        });
    }
}

impl eframe::App for Panel {
    fn ui(&mut self, ui: &mut egui::Ui, _frame: &mut eframe::Frame) {
        let ctx = ui.ctx().clone();
        ctx.request_repaint_after(Duration::from_secs(1));
        let (scan, busy, note, probe) = {
            let sh = self.shared.lock().unwrap();
            (sh.scan.clone(), sh.busy.clone(), sh.note.clone(), sh.probe.clone())
        };
        let free = busy.is_none();
        let any_running = scan.tickets.iter().any(|t| t.status == "running" || t.status == "waiting");

        egui::Panel::top("bar").show(ui, |ui| {
            ui.add_space(6.0);
            ui.horizontal(|ui| {
                ui.heading("Верстак");
                ui.add_space(16.0);
                let (c, text) = match (scan.checked, scan.docker) {
                    (false, _) => (Color32::GRAY, "Docker: проверяю"),
                    (true, true) => (Color32::from_rgb(70, 170, 90), "Docker работает"),
                    (true, false) => (Color32::from_rgb(220, 80, 70), "Docker не запущен"),
                };
                ui.label(RichText::new("●").color(c));
                ui.label(text);
                if scan.checked && !scan.docker && ui.add_enabled(free, egui::Button::new("Запустить Docker")).clicked() {
                    self.run(&ctx, "Запускаю Docker Desktop", || match Command::new(DOCKER_DESKTOP).spawn() {
                        Ok(_) => "Docker Desktop запускается, это до минуты".into(),
                        Err(e) => format!("не запустить Docker Desktop: {e}"),
                    });
                }
                ui.add_space(16.0);
                if ui
                    .add_enabled(free && scan.docker && !any_running, egui::Button::new("Проверить изоляцию"))
                    .on_hover_text("9 проверок изнутри контейнера, собранного как для тикета: нет root, дисков, папок хоста, выхода в сеть мимо прокси")
                    .clicked()
                {
                    let (exe, cfg, root, shared) =
                        (self.root.join("bin").join("verstak.exe"), self.root.join("verstak.json"), self.root.clone(), self.shared.clone());
                    self.show_probe = true;
                    self.run(&ctx, "Проверяю изоляцию", move || {
                        let out = Command::new(&exe).arg("-config").arg(&cfg).arg("-probe").current_dir(&root).creation_flags(NO_WINDOW).output();
                        let (text, ok) = match out {
                            Ok(o) => (format!("{}{}", String::from_utf8_lossy(&o.stdout), String::from_utf8_lossy(&o.stderr)), o.status.success()),
                            Err(e) => (e.to_string(), false),
                        };
                        shared.lock().unwrap().probe = Some(text);
                        if ok { "изоляция: все проверки пройдены".into() } else { "изоляция: есть провалы, см. окно".into() }
                    });
                }
                if ui.button("Папка тикетов").clicked() {
                    let _ = Command::new("explorer").arg(&self.work).spawn();
                }
                ui.add_space(12.0);
                if let Some(b) = &busy {
                    ui.spinner();
                    ui.label(format!("{b}…"));
                } else if !note.is_empty() {
                    ui.label(RichText::new(&note).weak());
                }
            });
            ui.add_space(6.0);
        });

        egui::Panel::left("tickets").resizable(true).default_size(330.0).show(ui, |ui| {
            ui.add_space(6.0);
            let can = free && scan.docker && !any_running;
            let label = if self.form_open { "Скрыть форму" } else { "+ Новый тикет" };
            if ui
                .add_enabled(can || self.form_open, egui::Button::new(RichText::new(label).strong()))
                .on_disabled_hover_text("Один тикет за раз, и только когда Docker работает")
                .clicked()
            {
                self.form_open = !self.form_open;
            }
            if self.form_open {
                ui.add_space(4.0);
                egui::ComboBox::from_label("цель").selected_text(&self.form_target).show_ui(ui, |ui| {
                    for t in &self.targets {
                        ui.selectable_value(&mut self.form_target, t.clone(), t);
                    }
                });
                ui.add(egui::TextEdit::singleline(&mut self.form_title).hint_text("что не так или что сделать").desired_width(f32::INFINITY));
                ui.add(
                    egui::TextEdit::multiline(&mut self.form_body)
                        .hint_text("подробности: где, что ожидается")
                        .desired_rows(6)
                        .desired_width(f32::INFINITY),
                );
                if ui.add_enabled(can && !self.form_title.trim().is_empty(), egui::Button::new("В работу")).clicked() {
                    self.submit(&ctx);
                }
            }
            ui.separator();
            egui::ScrollArea::vertical().auto_shrink([false, false]).show(ui, |ui| {
                if scan.tickets.is_empty() {
                    ui.label(RichText::new("тикетов пока нет").weak());
                }
                for t in &scan.tickets {
                    let (c, word) = status_look(&t.status);
                    let picked = self.selected.as_deref() == Some(t.id.as_str());
                    let resp = ui.add(egui::Button::selectable(picked, RichText::new(&t.title)).wrap_mode(egui::TextWrapMode::Truncate));
                    if resp.on_hover_text(&t.id).clicked() {
                        self.selected = Some(t.id.clone());
                    }
                    ui.horizontal(|ui| {
                        ui.add_space(6.0);
                        ui.label(RichText::new(word).color(c).small());
                        ui.label(RichText::new(when(&t.id)).small().weak());
                    });
                    ui.add_space(4.0);
                }
            });
        });

        if self.selected.is_none() {
            self.selected = scan.tickets.first().map(|t| t.id.clone()); // the newest
        }
        let current = self.selected.as_ref().and_then(|id| scan.tickets.iter().find(|t| &t.id == id)).cloned();
        egui::CentralPanel::default().show(ui, |ui| {
            let Some(t) = current else {
                ui.add_space(24.0);
                ui.label(RichText::new("Выбери тикет слева или заведи новый.").weak());
                return;
            };
            self.read_view(&t);
            let (c, word) = status_look(&t.status);
            ui.add_space(4.0);
            ui.horizontal_wrapped(|ui| {
                ui.heading(&t.title);
                ui.label(RichText::new(word).color(c).strong());
            });
            ui.horizontal_wrapped(|ui| {
                let (i, cached, o) = t.tokens;
                let share = if i > 0 { cached * 100 / i } else { 0 };
                ui.label(RichText::new(format!(
                    "раундов {} · {} · токены: {i} вход ({share}% из кэша), {o} выход",
                    t.rounds,
                    clock(t.took_s)
                ))
                .weak());
            });
            if !t.err.is_empty() {
                ui.label(RichText::new(&t.err).color(Color32::from_rgb(220, 80, 70)).small());
            }
            ui.horizontal(|ui| {
                let running = scan.running.contains(&format!("verstak-{}", t.id));
                if ui.add_enabled(free && running, egui::Button::new("■ Стоп")).on_hover_text("Убить контейнер тикета; результаты, какие есть, заберутся").clicked() {
                    let name = format!("verstak-{}", t.id);
                    self.run(&ctx, "Останавливаю контейнер", move || {
                        let ok = Command::new("docker").args(["kill", &name]).creation_flags(NO_WINDOW).output().map(|o| o.status.success()).unwrap_or(false);
                        if ok { "контейнер остановлен".into() } else { "не остановить контейнер".into() }
                    });
                }
                let can_take = free && t.status == "done" && !t.branch.is_empty();
                if ui
                    .add_enabled(can_take, egui::Button::new("Забрать ветку в верстак"))
                    .on_hover_text("git fetch из приёмника в основной репозиторий верстака: появится локальная ветка, сливать — самому после чтения diff")
                    .clicked()
                {
                    let (root, inbox, branch) = (self.root.clone(), self.work.join("inbox.git"), t.branch.clone());
                    self.run(&ctx, "Забираю ветку", move || take_branch(&root, &inbox, &branch));
                }
                if ui.button("Папка").clicked() {
                    let _ = Command::new("explorer").arg(&t.dir).spawn();
                }
            });
            ui.separator();
            ui.horizontal(|ui| {
                ui.selectable_value(&mut self.tab, Tab::Progress, "Ход работы");
                ui.selectable_value(&mut self.tab, Tab::Report, "Отчёт");
                ui.selectable_value(&mut self.tab, Tab::Changes, "Изменения");
                ui.selectable_value(&mut self.tab, Tab::Steps, "Шаги");
                ui.selectable_value(&mut self.tab, Tab::Network, "Сеть");
                ui.selectable_value(&mut self.tab, Tab::Ticket, "Тикет");
            });
            ui.separator();
            let stick = self.tab == Tab::Progress;
            egui::ScrollArea::both().stick_to_bottom(stick).auto_shrink([false, false]).show(ui, |ui| match self.tab {
                Tab::Progress => {
                    if self.view.events.is_empty() {
                        ui.label(RichText::new("событий пока нет").weak());
                    }
                    for (k, line) in &self.view.events {
                        ui.label(RichText::new(line).monospace().size(12.0).color(kind_color(*k, ui)));
                    }
                }
                Tab::Report => mono(ui, &self.view.report, "отчёта пока нет"),
                Tab::Changes => {
                    if self.view.changes.is_empty() {
                        ui.label(RichText::new("изменений нет").weak());
                    }
                    for l in self.view.changes.lines() {
                        let c = if l.starts_with('+') && !l.starts_with("+++") {
                            Color32::from_rgb(70, 170, 90)
                        } else if l.starts_with('-') && !l.starts_with("---") {
                            Color32::from_rgb(220, 80, 70)
                        } else {
                            ui.visuals().text_color()
                        };
                        ui.label(RichText::new(l).monospace().size(12.0).color(c));
                    }
                }
                Tab::Steps => {
                    egui::Grid::new("steps").striped(true).num_columns(4).show(ui, |ui| {
                        for h in ["шаг", "начало", "длилось", "подробности"] {
                            ui.label(RichText::new(h).strong());
                        }
                        ui.end_row();
                        for s in &self.view.steps {
                            ui.label(if s.ok { RichText::new(&s.name) } else { RichText::new(&s.name).color(Color32::from_rgb(220, 80, 70)) });
                            ui.label(clock(s.at_ms / 1000));
                            ui.label(format!("{:.1} с", s.ms as f64 / 1000.0));
                            ui.label(RichText::new(&s.extra).weak());
                            ui.end_row();
                        }
                    });
                }
                Tab::Network => {
                    ui.label(RichText::new("Куда агент выходил через прокси: пропущено / отклонено").weak());
                    egui::Grid::new("net").striped(true).num_columns(3).show(ui, |ui| {
                        for (host, ok, no) in &self.view.network {
                            ui.label(RichText::new(host).monospace());
                            ui.label(format!("{ok}"));
                            ui.label(if *no > 0 { RichText::new(no.to_string()).color(Color32::from_rgb(210, 160, 40)) } else { RichText::new("0").weak() });
                            ui.end_row();
                        }
                    });
                }
                Tab::Ticket => mono(ui, &self.view.ticket, ""),
            });
        });

        if self.show_probe {
            let mut open = true;
            egui::Window::new("Проверка изоляции").open(&mut open).default_width(640.0).show(&ctx, |ui| match &probe {
                None => {
                    ui.spinner();
                }
                Some(text) => {
                    for l in text.lines() {
                        let c = if l.starts_with("FAIL") || l.contains("failed") {
                            Color32::from_rgb(220, 80, 70)
                        } else if l.starts_with("pass") {
                            Color32::from_rgb(70, 170, 90)
                        } else {
                            ui.visuals().text_color()
                        };
                        ui.label(RichText::new(l).monospace().size(12.0).color(c));
                    }
                }
            });
            self.show_probe = open;
        }
    }
}

fn mono(ui: &mut egui::Ui, text: &str, empty: &str) {
    if text.trim().is_empty() {
        ui.label(RichText::new(empty).weak());
        return;
    }
    for l in text.lines() {
        ui.label(RichText::new(l).monospace().size(12.0));
    }
}

/// How the list shows a status.
fn status_look(s: &str) -> (Color32, &'static str) {
    match s {
        "done" => (Color32::from_rgb(70, 170, 90), "готово"),
        "failed" => (Color32::from_rgb(220, 80, 70), "красное"),
        "no-change" => (Color32::GRAY, "без правок"),
        "agent-down" => (Color32::from_rgb(210, 160, 40), "агент упал"),
        "running" => (Color32::from_rgb(80, 150, 230), "в работе"),
        "waiting" => (Color32::from_rgb(80, 150, 230), "готовится"),
        _ => (Color32::GRAY, "?"),
    }
}

/// Kinds of lines in the progress view.
#[derive(Clone, Copy, Debug, PartialEq)]
enum Kind {
    Agent,
    Think,
    Command,
    File,
    Step,
    Error,
    Plain,
}

fn kind_color(k: Kind, ui: &egui::Ui) -> Color32 {
    match k {
        Kind::Agent => ui.visuals().strong_text_color(),
        Kind::Think => ui.visuals().weak_text_color(),
        Kind::Command => Color32::from_rgb(80, 150, 230),
        Kind::File => Color32::from_rgb(70, 170, 90),
        Kind::Step => Color32::from_rgb(170, 120, 210),
        Kind::Error => Color32::from_rgb(220, 80, 70),
        Kind::Plain => ui.visuals().text_color(),
    }
}

/// One line of the container's log as the progress view shows it: codex's JSON events, the
/// workbench's steps, and plain lines. None hides it.
fn event_line(l: &str) -> Option<(Kind, String)> {
    let l = l.trim();
    if l.is_empty() {
        return None;
    }
    let Ok(v) = serde_json::from_str::<Value>(l) else { return Some((Kind::Plain, l.to_string())) };
    if let Some(span) = v.get("span").and_then(Value::as_str) {
        let ms = v.get("ms").and_then(Value::as_u64).unwrap_or(0);
        let ok = v.get("ok").and_then(Value::as_bool).unwrap_or(true);
        let mut s = format!("⏱ {span} — {:.1} с", ms as f64 / 1000.0);
        if let Some(p) = v.get("passed").and_then(Value::as_bool) {
            s += if p { " · проверки зелёные" } else { " · проверки КРАСНЫЕ" };
        }
        if let Some(st) = v.get("status").and_then(Value::as_str) {
            s += &format!(" · {st}");
        }
        if let Some(e) = v.get("err").and_then(Value::as_str) {
            s += &format!(" · {e}");
        }
        return Some((if ok { Kind::Step } else { Kind::Error }, s));
    }
    let typ = v.get("type").and_then(Value::as_str).unwrap_or("");
    match typ {
        "thread.started" => Some((Kind::Step, "сессия агента начата".into())),
        "turn.started" | "item.updated" => None,
        "turn.completed" => {
            let u = &v["usage"];
            let n = |k: &str| u.get(k).and_then(Value::as_u64).unwrap_or(0);
            Some((Kind::Step, format!("ход окончен: {} вход ({} из кэша), {} выход", n("input_tokens"), n("cached_input_tokens"), n("output_tokens"))))
        }
        "turn.failed" | "error" => {
            let msg = v.pointer("/error/message").or_else(|| v.get("message")).and_then(Value::as_str).unwrap_or(l);
            Some((Kind::Error, format!("ошибка: {msg}")))
        }
        "item.started" | "item.completed" => item_line(&v["item"], typ == "item.completed"),
        _ => None,
    }
}

fn item_line(item: &Value, done: bool) -> Option<(Kind, String)> {
    let s = |k: &str| item.get(k).and_then(Value::as_str).unwrap_or("");
    match s("type") {
        "agent_message" if done => Some((Kind::Agent, format!("агент: {}", s("text")))),
        "reasoning" if done => Some((Kind::Think, format!("думает: {}", s("text")))),
        "command_execution" => {
            if !done {
                return Some((Kind::Command, format!("$ {}", s("command"))));
            }
            let code = item.get("exit_code").and_then(Value::as_i64);
            let out = s("aggregated_output").lines().last().unwrap_or("").to_string();
            match code {
                Some(0) => None, // the start line is enough for a command that went well
                Some(c) => Some((Kind::Error, format!("  ↳ код {c}: {out}"))),
                None => None,
            }
        }
        "file_change" if done => {
            let files: Vec<String> = item
                .get("changes")
                .and_then(Value::as_array)
                .map(|a| a.iter().map(|c| format!("{} ({})", c["path"].as_str().unwrap_or("?"), c["kind"].as_str().unwrap_or("?"))).collect())
                .unwrap_or_default();
            Some((Kind::File, format!("правит: {}", files.join(", "))))
        }
        "error" => Some((Kind::Error, format!("ошибка: {}", s("message")))),
        "" => None,
        other if done => Some((Kind::Plain, format!("[{other}]"))),
        _ => None,
    }
}

/// A step of the trace.
#[derive(Clone, Debug, PartialEq)]
struct Step {
    name: String,
    at_ms: u64,
    ms: u64,
    ok: bool,
    extra: String,
}

fn step_line(l: &str) -> Option<Step> {
    let v: Value = serde_json::from_str(l.trim()).ok()?;
    let name = v.get("span")?.as_str()?.to_string();
    let mut extra = Vec::new();
    if let Some(o) = v.as_object() {
        for (k, x) in o {
            if !matches!(k.as_str(), "span" | "at_ms" | "ms" | "ok") {
                extra.push(format!("{k}={}", x.as_str().map(str::to_string).unwrap_or_else(|| x.to_string())));
            }
        }
    }
    Some(Step {
        name,
        at_ms: v.get("at_ms").and_then(Value::as_u64).unwrap_or(0),
        ms: v.get("ms").and_then(Value::as_u64).unwrap_or(0),
        ok: v.get("ok").and_then(Value::as_bool).unwrap_or(true),
        extra: extra.join(" "),
    })
}

/// The proxy's log, per host: tunnels opened and requests refused.
fn network(log: &str) -> Vec<(String, u64, u64)> {
    let mut m: BTreeMap<String, (u64, u64)> = BTreeMap::new();
    for l in log.lines() {
        let Ok(v) = serde_json::from_str::<Value>(l.trim()) else { continue };
        let host = v.get("host").and_then(Value::as_str).unwrap_or("").trim_end_matches(":443").to_string();
        if host.is_empty() {
            continue;
        }
        match v.get("msg").and_then(Value::as_str) {
            Some("tunnel") => m.entry(host).or_default().0 += 1,
            Some("denied") => m.entry(host).or_default().1 += 1,
            _ => {}
        }
    }
    m.into_iter().map(|(h, (a, b))| (h, a, b)).collect()
}

/// The tickets in the work folder, newest first, and which containers run.
fn scan(work: &Path) -> Scan {
    let mut s = Scan { checked: true, ..Default::default() };
    if let Ok(o) = Command::new("docker").args(["ps", "--format", "{{.Names}}"]).creation_flags(NO_WINDOW).output() {
        s.docker = o.status.success();
        s.running = String::from_utf8_lossy(&o.stdout).lines().map(|l| l.trim().to_string()).collect();
    }
    let Ok(entries) = fs::read_dir(work) else { return s };
    for e in entries.flatten() {
        let dir = e.path();
        let Ok(spec) = fs::read_to_string(dir.join("in").join("job.json")) else { continue };
        let id = e.file_name().to_string_lossy().into_owned();
        let mut t = ticket_from(&spec, fs::read_to_string(dir.join("out").join("report.json")).ok().as_deref());
        if t.status.is_empty() {
            t.status = if s.running.contains(&format!("verstak-{id}")) { "running" } else { "waiting" }.into();
        }
        t.id = id;
        t.dir = dir;
        s.tickets.push(t);
    }
    s.tickets.sort_by(|a, b| b.id.cmp(&a.id));
    s
}

/// A ticket from its spec (in/job.json) and, when there is one, its report (out/report.json).
fn ticket_from(spec: &str, report: Option<&str>) -> Ticket {
    let mut t = Ticket::default();
    if let Ok(v) = serde_json::from_str::<Value>(spec) {
        t.title = v.pointer("/ticket/Title").and_then(Value::as_str).unwrap_or("").to_string();
    }
    if let Some(r) = report.and_then(|r| serde_json::from_str::<Value>(r).ok()) {
        let n = |p: &str| r.pointer(p).and_then(Value::as_u64).unwrap_or(0);
        t.status = r.get("Status").and_then(Value::as_str).unwrap_or("").to_string();
        t.rounds = n("/Rounds");
        t.took_s = n("/Took") / 1_000_000_000;
        t.tokens = (n("/Usage/input_tokens"), n("/Usage/cached_input_tokens"), n("/Usage/output_tokens"));
        if r.get("Commits").and_then(Value::as_str).is_some_and(|c| !c.is_empty()) {
            t.branch = r.get("Branch").and_then(Value::as_str).unwrap_or("").to_string();
        }
        t.err = r.get("Err").and_then(Value::as_str).unwrap_or("").to_string();
    }
    if t.title.is_empty() {
        t.title = "(без названия)".into();
    }
    t
}

/// Fetch a finished branch from the inbox into the workbench's own repo, hooks off. Nothing is
/// merged: the developer reads the diff and merges.
fn take_branch(root: &Path, inbox: &Path, branch: &str) -> String {
    let out = Command::new("git")
        .args(["-c", "core.hooksPath=NUL", "-c", "core.fsmonitor=false", "-C"])
        .arg(root)
        .arg("fetch")
        .arg(inbox)
        .arg(format!("{branch}:{branch}"))
        .creation_flags(NO_WINDOW)
        .output();
    match out {
        Ok(o) if o.status.success() => format!("ветка {branch} в {}: смотри diff и сливай сам", root.display()),
        Ok(o) => format!("git fetch: {}", String::from_utf8_lossy(&o.stderr).trim()),
        Err(e) => format!("git: {e}"),
    }
}

fn git_out(repo: &Path, args: &[&str]) -> String {
    Command::new("git")
        .args(["-c", "core.hooksPath=NUL", "-C"])
        .arg(repo)
        .args(args)
        .creation_flags(NO_WINDOW)
        .output()
        .map(|o| String::from_utf8_lossy(&o.stdout).into_owned())
        .unwrap_or_default()
}

/// verstak.json: the work folder and the target names.
fn read_config(path: &Path) -> (PathBuf, Vec<String>) {
    let v: Value = fs::read_to_string(path).ok().and_then(|s| serde_json::from_str(&s).ok()).unwrap_or(Value::Null);
    let work = v.get("work").and_then(Value::as_str).map(PathBuf::from).unwrap_or_else(|| PathBuf::from(r"F:\verstak-work"));
    let mut targets: Vec<String> = v.get("targets").and_then(Value::as_object).map(|o| o.keys().cloned().collect()).unwrap_or_default();
    targets.sort();
    (work, targets)
}

fn find_root() -> PathBuf {
    if let Ok(exe) = std::env::current_exe() {
        for dir in exe.ancestors().skip(1) {
            if dir.join("go.mod").exists() && dir.join("workplace").exists() {
                return dir.to_path_buf();
            }
        }
    }
    PathBuf::from(r"F:\verstak")
}

fn tail(path: &Path, max: u64) -> String {
    let Ok(mut f) = fs::File::open(path) else { return String::new() };
    let len = f.metadata().map(|m| m.len()).unwrap_or(0);
    let start = len.saturating_sub(max);
    if f.seek(SeekFrom::Start(start)).is_err() {
        return String::new();
    }
    let mut buf = Vec::new();
    let _ = f.read_to_end(&mut buf);
    let text = String::from_utf8_lossy(&buf).into_owned();
    if start > 0 {
        // The first line is cut in the middle: drop it.
        return text.split_once('\n').map(|(_, rest)| rest.to_string()).unwrap_or_default();
    }
    text
}

fn clock(secs: u64) -> String {
    if secs >= 3600 {
        format!("{}:{:02}:{:02}", secs / 3600, secs / 60 % 60, secs % 60)
    } else {
        format!("{}:{:02}", secs / 60, secs % 60)
    }
}

/// "20261021-024310-slug" -> "21.10 02:43".
fn when(id: &str) -> String {
    let b = id.as_bytes();
    if b.len() < 13 || b[8] != b'-' || !b[..8].iter().chain(&b[9..13]).all(u8::is_ascii_digit) {
        return String::new();
    }
    format!("{}.{} {}:{}", &id[6..8], &id[4..6], &id[9..11], &id[11..13])
}

/// A name for a new ticket's file, sortable by time.
fn now_stamp() -> String {
    let secs = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    format!("ticket-{secs}")
}

/// Segoe UI and Consolas from Windows, ahead of the bundled egui fonts.
fn setup_fonts(ctx: &egui::Context) {
    let mut fonts = egui::FontDefinitions::default();
    for (name, file, family) in [
        ("segoe", r"C:\Windows\Fonts\segoeui.ttf", egui::FontFamily::Proportional),
        ("consolas", r"C:\Windows\Fonts\consola.ttf", egui::FontFamily::Monospace),
    ] {
        if let Ok(bytes) = fs::read(file) {
            fonts.font_data.insert(name.into(), Arc::new(egui::FontData::from_owned(bytes)));
            fonts.families.entry(family).or_default().insert(0, name.into());
        }
    }
    ctx.set_fonts(fonts);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn codex_events_read_as_progress() {
        let cases = [
            (r#"{"type":"thread.started","thread_id":"t1"}"#, Some(Kind::Step)),
            (r#"{"type":"turn.started"}"#, None),
            (r#"{"type":"item.completed","item":{"type":"agent_message","text":"Сначала прочту тикет"}}"#, Some(Kind::Agent)),
            (r#"{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}"#, Some(Kind::Think)),
            (r#"{"type":"item.started","item":{"type":"command_execution","command":"go test ./...","status":"in_progress"}}"#, Some(Kind::Command)),
            (r#"{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","exit_code":0,"aggregated_output":"ok"}}"#, None),
            (r#"{"type":"item.completed","item":{"type":"command_execution","command":"go test","exit_code":1,"aggregated_output":"a\nFAIL x"}}"#, Some(Kind::Error)),
            (r#"{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"a.go","kind":"update"}]}}"#, Some(Kind::File)),
            (r#"{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":6,"output_tokens":2}}"#, Some(Kind::Step)),
            (r#"{"type":"error","message":"401 Unauthorized"}"#, Some(Kind::Error)),
            (r#"{"span":"checks","at_ms":10,"ms":1500,"ok":true,"passed":false}"#, Some(Kind::Step)),
            (r#"{"span":"agent","at_ms":10,"ms":1500,"ok":false,"err":"codex: exit 1"}"#, Some(Kind::Error)),
            ("inside: done after 1 round(s)", Some(Kind::Plain)),
            ("   ", None),
        ];
        for (line, want) in cases {
            assert_eq!(event_line(line).map(|(k, _)| k), want, "{line}");
        }
        let (_, text) = event_line(cases[2].0).unwrap();
        assert_eq!(text, "агент: Сначала прочту тикет");
        let (_, text) = event_line(cases[6].0).unwrap();
        assert_eq!(text, "  ↳ код 1: FAIL x");
        let (_, text) = event_line(cases[10].0).unwrap();
        assert!(text.contains("КРАСНЫЕ"), "{text}");
    }

    #[test]
    fn steps_keep_their_extras() {
        let s = step_line(r#"{"span":"agent","at_ms":2500,"ms":61000,"ok":true,"round":1,"in":900}"#).unwrap();
        assert_eq!((s.name.as_str(), s.at_ms, s.ms, s.ok), ("agent", 2500, 61000, true));
        assert_eq!(s.extra, "in=900 round=1");
        assert!(step_line(r#"{"type":"turn.started"}"#).is_none());
        assert!(step_line("plain").is_none());
    }

    #[test]
    fn network_counts_per_host() {
        let log = r#"{"msg":"egress up","listen":":3128"}
{"msg":"tunnel","host":"chatgpt.com:443"}
{"msg":"tunnel","host":"chatgpt.com:443"}
{"msg":"denied","host":"github.com:443","why":"not on the list"}
{"msg":"upstream","host":"api.openai.com:443","err":"dns"}
garbage"#;
        assert_eq!(network(log), vec![("chatgpt.com".into(), 2, 0), ("github.com".into(), 0, 1)]);
    }

    #[test]
    fn tickets_from_spec_and_report() {
        let spec = r#"{"ticket":{"ID":"x","Title":"Тесты для check"},"base":"main"}"#;
        let t = ticket_from(spec, None);
        assert_eq!((t.title.as_str(), t.status.as_str()), ("Тесты для check", ""));
        let report = r#"{"Status":"done","Rounds":2,"Took":125000000000,"Usage":{"input_tokens":1000,"cached_input_tokens":700,"output_tokens":50},"Branch":"verstak/x","Commits":"abc fix","Err":""}"#;
        let t = ticket_from(spec, Some(report));
        assert_eq!((t.status.as_str(), t.rounds, t.took_s, t.tokens), ("done", 2, 125, (1000, 700, 50)));
        assert_eq!(t.branch, "verstak/x");
        // No commits: nothing to take.
        let t = ticket_from(spec, Some(r#"{"Status":"failed","Branch":"verstak/x","Commits":""}"#));
        assert_eq!(t.branch, "");
        assert_eq!(ticket_from("not json", None).title, "(без названия)");
    }

    #[test]
    fn when_reads_the_id() {
        assert_eq!(when("20261021-024310-u-paketov"), "21.10 02:43");
        assert_eq!(when("ticket-123"), "");
        assert_eq!(when("2026102x-024310-a"), "");
    }

    #[test]
    fn clock_reads() {
        assert_eq!(clock(5), "0:05");
        assert_eq!(clock(125), "2:05");
        assert_eq!(clock(3725), "1:02:05");
    }
}
