import { FormEvent, ReactNode, useEffect, useMemo, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import {
  AlertTriangle,
  Check,
  ChevronRight,
  CircleUserRound,
  Cpu,
  Download,
  ExternalLink,
  Folder,
  Gamepad2,
  Gauge,
  HardDrive,
  LogOut,
  MapPin,
  MessageCircle,
  Newspaper,
  Play,
  RefreshCw,
  Server,
  Settings,
  ShieldAlert,
  Sparkles,
  X,
} from "lucide-react";

type NewsItem = { title: string; date: string; body: string };

type LauncherState = {
  apiUrl: string;
  serverNames: string[];
  serverIndex: number;
  serverVisible: boolean;
  message: string;
  userLogin: string;
  userUuid: string;
  tokenExpiresAt: string;
  requiresTotp: boolean;
  sessionRestoring: boolean;
  isLoading: boolean;
  isAuthenticated: boolean;
  isSlim: boolean;
  isSyncing: boolean;
  hasProfile: boolean;
  profileInstalled: boolean;
  profileUpdateAvailable: boolean;
  profileStateChecking: boolean;
  profileStateUnknown: boolean;
  downloadPanelVisible: boolean;
  selectedProfileName: string;
  selectedProfileVersion: string;
  profileStatus: string;
  playtimeTotal: string;
  downloadPhase: string;
  downloadFile: string;
  downloadCounter: string;
  downloadProgress: number;
  settingsVisible: boolean;
  memoryGb: number;
  memoryMax: number;
  memoryAuto: boolean;
  memoryLabel: string;
  discreteGpuAvailable: boolean;
  discreteGpuLabel: string;
  useDiscreteGpu: boolean;
  discordRpcEnabled: boolean;
  installFolder: string;
  newsItems: NewsItem[];
  anticheatAlert: string;
  updateReady: boolean;
  updateMandatory: boolean;
  updateVersion: string;
  updateStatus: string;
  policyVisible: boolean;
  policyAccepting: boolean;
  policyText: string;
  policyVersionLabel: string;
  policyVersion: number;
};

const emptyState: LauncherState = {
  apiUrl: "",
  serverNames: [],
  serverIndex: 0,
  serverVisible: false,
  message: "Запускаем лаунчер…",
  userLogin: "",
  userUuid: "",
  tokenExpiresAt: "",
  requiresTotp: false,
  sessionRestoring: false,
  isLoading: true,
  isAuthenticated: false,
  isSlim: false,
  isSyncing: false,
  hasProfile: false,
  profileInstalled: false,
  profileUpdateAvailable: false,
  profileStateChecking: false,
  profileStateUnknown: false,
  downloadPanelVisible: false,
  selectedProfileName: "",
  selectedProfileVersion: "-",
  profileStatus: "Offline",
  playtimeTotal: "< 1 мин",
  downloadPhase: "",
  downloadFile: "",
  downloadCounter: "",
  downloadProgress: 0,
  settingsVisible: false,
  memoryGb: 8,
  memoryMax: 64,
  memoryAuto: true,
  memoryLabel: "Авто · 8 ГБ",
  discreteGpuAvailable: false,
  discreteGpuLabel: "",
  useDiscreteGpu: true,
  discordRpcEnabled: true,
  installFolder: "",
  newsItems: [],
  anticheatAlert: "",
  updateReady: false,
  updateMandatory: false,
  updateVersion: "",
  updateStatus: "",
  policyVisible: false,
  policyAccepting: false,
  policyText: "",
  policyVersionLabel: "",
  policyVersion: 0,
};

const browserPreview: LauncherState = {
  ...emptyState,
  apiUrl: "https://launcher.likonchik.xyz",
  serverNames: ["Авто — основной · 42 мс", "Зеркало · 71 мс"],
  serverVisible: true,
  isLoading: false,
  isAuthenticated: true,
  message: "Профиль проверен. Можно запускать игру.",
  userLogin: "Likonchik",
  userUuid: "2bdc44c5-9c18-42d1-a2cb-b0977924fd18",
  tokenExpiresAt: "2026-09-19 14:40",
  hasProfile: true,
  profileInstalled: true,
  selectedProfileName: "Project Minecraft: Warfare",
  selectedProfileVersion: "1.21.1 · NeoForge",
  profileStatus: "Готов к запуску",
  playtimeTotal: "38 ч 24 мин",
  installFolder: "/home/player/Project Minecraft",
  newsItems: [
    { title: "Операция «Рубеж» уже в игре", date: "20 августа", body: "Новый сценарий вторжения, мобильные точки снабжения и обновлённые роли отрядов." },
    { title: "Лаунчер получил новый интерфейс", date: "сегодня", body: "Установка, обновления и диагностика теперь собраны в одной линии готовности." },
  ],
};

function isTauriRuntime() {
  return "__TAURI_INTERNALS__" in window;
}

function browserPreviewState(): LauncherState {
  const screen = new URLSearchParams(window.location.search).get("screen");
  if (screen === "login") {
    return { ...browserPreview, isAuthenticated: false, isLoading: false, message: "Готов к входу." };
  }
  if (screen === "restore") {
    return { ...browserPreview, isAuthenticated: false, sessionRestoring: true, isLoading: true, message: "Измеряем задержку серверов…" };
  }
  if (screen === "settings") return { ...browserPreview, settingsVisible: true, discreteGpuAvailable: true, discreteGpuLabel: "NVIDIA GeForce RTX" };
  if (screen === "policy") return { ...browserPreview, policyVisible: true, policyVersionLabel: "Версия 4", policyText: "Project Minecraft обрабатывает данные аккаунта и технические сведения, необходимые для запуска игры и работы системы защиты.\n\nПродолжая, вы подтверждаете согласие с актуальной политикой конфиденциальности." };
  return browserPreview;
}

async function action(name: string, payload: Record<string, unknown> = {}) {
  if (!isTauriRuntime()) return;
  await invoke("launcher_action", { action: name, payload });
}

function useLauncherState() {
  const [state, setState] = useState<LauncherState>(isTauriRuntime() ? emptyState : browserPreviewState);

  useEffect(() => {
    if (!isTauriRuntime()) return;
    let disposed = false;
    const unlisten = listen<LauncherState>("launcher-state", (event) => {
      if (!disposed) setState(event.payload);
    });
    invoke<LauncherState>("get_launcher_state")
      .then((next) => !disposed && setState(next))
      .catch(() => undefined);
    return () => {
      disposed = true;
      void unlisten.then((stop) => stop());
    };
  }, []);

  return state;
}

function IconButton({ label, children, onClick, danger = false }: { label: string; children: ReactNode; onClick: () => void; danger?: boolean }) {
  return (
    <button className={`icon-button${danger ? " danger" : ""}`} onClick={onClick} aria-label={label} title={label}>
      {children}
    </button>
  );
}

function StatusDot({ active = true }: { active?: boolean }) {
  return <span className={`status-dot${active ? " active" : ""}`} aria-hidden="true" />;
}

function LoginScreen({ state }: { state: LauncherState }) {
  const [login, setLogin] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");

  const submit = (event: FormEvent) => {
    event.preventDefault();
    void action("login", { login, password, totp });
    if (totp) setTotp("");
  };

  return (
    <main className="login-layout">
      <section className="login-thesis">
        <div className="brand-lockup">
          <span className="brand-mark"><MapPin size={22} /></span>
          <div><b>PROJECT MINECRAFT</b><span>PLAYER LAUNCH SYSTEM</span></div>
        </div>
        <div className="coordinates">44°57′ N / 34°06′ E</div>
        <h1>Точка входа<br />в вашу <em>операцию.</em></h1>
        <p>Лаунчер проверит сборку, доставит обновления и подготовит защищённую игровую сессию.</p>
        <div className="system-line"><StatusDot /><span>{state.apiUrl || "Подключение к серверу"}</span></div>
      </section>

      <form className="login-card" onSubmit={submit}>
        <div className="card-kicker"><span>Авторизация</span><ShieldAlert size={18} /></div>
        <h2>С возвращением</h2>
        <p className="muted">Используйте аккаунт Project Minecraft.</p>

        <label>
          <span>Логин</span>
          <input autoFocus autoComplete="username" value={login} onChange={(event) => setLogin(event.target.value)} disabled={state.isLoading} placeholder="Ваш логин" />
        </label>
        <label>
          <span>Пароль</span>
          <input type="password" autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} disabled={state.isLoading} placeholder="••••••••" />
        </label>
        {state.requiresTotp && (
          <label className="totp-field">
            <span>Код подтверждения</span>
            <input inputMode="numeric" autoComplete="one-time-code" maxLength={6} value={totp} onChange={(event) => setTotp(event.target.value.replace(/\D/g, ""))} disabled={state.isLoading} placeholder="000 000" />
          </label>
        )}

        {state.serverVisible && (
          <label>
            <span>Сервер подключения</span>
            <select value={state.serverIndex} onChange={(event) => void action("selectServer", { index: Number(event.target.value) })} disabled={state.isLoading}>
              {state.serverNames.map((name, index) => <option key={`${name}-${index}`} value={index}>{name}</option>)}
            </select>
          </label>
        )}

        <button className="primary-button" type="submit" disabled={state.isLoading || !login.trim() || !password}>
          {state.isLoading ? <RefreshCw className="spin" size={20} /> : <ChevronRight size={20} />}
          {state.isLoading ? "Проверяем аккаунт" : "Войти в лаунчер"}
        </button>
        <div className={`inline-message${state.message ? " visible" : ""}`}>{state.message}</div>
        <div className="login-links">
          <button type="button" onClick={() => void action("openUrl", { url: "https://t.me/project_minecraft" })}>Telegram</button>
          <span />
          <button type="button" onClick={() => void action("openUrl", { url: "https://pjm.likonchik.xyz" })}>Сайт проекта</button>
        </div>
      </form>
    </main>
  );
}

function RestoreScreen({ state }: { state: LauncherState }) {
  return (
    <main className="restore-layout" aria-live="polite">
      <div className="restore-radar" aria-hidden="true"><span /><i /><b /></div>
      <div className="brand-lockup">
        <span className="brand-mark"><MapPin size={22} /></span>
        <div><b>PROJECT MINECRAFT</b><span>PLAYER LAUNCH SYSTEM</span></div>
      </div>
      <section>
        <span className="eyebrow">Безопасный вход</span>
        <h1>Возвращаем<br />вашу сессию</h1>
        <p>{state.message}</p>
        <div className="restore-steps">
          <span className="done"><Check />Токен найден</span>
          <span className="active"><RefreshCw className="spin" />Ищем лучший сервер</span>
          <span><ShieldAlert />Проверяем профиль</span>
        </div>
      </section>
    </main>
  );
}

function Sidebar({ state, tab, setTab }: { state: LauncherState; tab: "home" | "news"; setTab: (tab: "home" | "news") => void }) {
  return (
    <aside className="sidebar">
      <div className="compact-brand"><span className="brand-mark"><MapPin size={18} /></span><div><b>PROJECT</b><span>MINECRAFT</span></div></div>
      <nav aria-label="Основная навигация">
        <button className={tab === "home" ? "active" : ""} onClick={() => setTab("home")}><Gamepad2 size={20} /><span>Играть</span></button>
        <button className={tab === "news" ? "active" : ""} onClick={() => setTab("news")}><Newspaper size={20} /><span>Новости</span>{state.newsItems.length > 0 && <b>{state.newsItems.length}</b>}</button>
        <button onClick={() => void action("openModsFolder")}><Folder size={20} /><span>Папка модов</span></button>
      </nav>
      <div className="sidebar-spacer" />
      <div className="connection-card"><StatusDot /><div><span>Подключение</span><b>{state.serverNames[state.serverIndex] || "Авто"}</b></div></div>
      <div className="sidebar-actions">
        <IconButton label="Настройки" onClick={() => void action("openSettings")}><Settings size={20} /></IconButton>
        <IconButton label="Выйти из аккаунта" danger onClick={() => void action("logout")}><LogOut size={20} /></IconButton>
      </div>
    </aside>
  );
}

function PlayButton({ state }: { state: LauncherState }) {
  const label = state.updateMandatory
    ? "Требуется обновление"
    : state.isSyncing
      ? state.downloadPhase || "Подготовка"
      : state.profileStateChecking
        ? "Проверяем файлы"
        : state.profileStateUnknown
          ? "Проверить сборку"
          : !state.profileInstalled
            ? "Установить сборку"
            : state.profileUpdateAvailable
              ? "Обновить сборку"
              : "Запустить игру";

  return (
    <button className="launch-button" onClick={() => void action("play")} disabled={!state.hasProfile || state.isSyncing || state.updateMandatory}>
      <span className="launch-button-icon">{state.isSyncing || state.profileStateChecking ? <RefreshCw className="spin" /> : state.profileInstalled ? <Play /> : <Download />}</span>
      <span><small>{state.isSyncing ? state.downloadCounter : "Основное действие"}</small><b>{label}</b></span>
      <ChevronRight />
    </button>
  );
}

function HomeView({ state }: { state: LauncherState }) {
  const steps = useMemo(() => [
    { label: "Профиль", value: state.hasProfile ? "Выбран" : "Не найден", done: state.hasProfile },
    { label: "Файлы", value: state.profileStatus, done: state.profileInstalled && !state.profileUpdateAvailable },
    { label: "Сессия", value: state.isAuthenticated ? "Активна" : "Не активна", done: state.isAuthenticated },
  ], [state]);

  return (
    <section className="home-view">
      <header className="topbar">
        <div><span className="eyebrow">Активный профиль</span><h1>{state.selectedProfileName || "Профиль не назначен"}</h1></div>
        <div className="account-pill"><CircleUserRound size={22} /><div><b>{state.userLogin}</b><span>Сессия до {state.tokenExpiresAt || "—"}</span></div></div>
      </header>

      <div className="mission-card">
        <div className="mission-visual">
          <div className="map-grid" />
          <div className="mission-copy"><span className="eyebrow">Сборка проекта</span><h2>{state.selectedProfileName || "Нет доступной сборки"}</h2><p>Minecraft {state.selectedProfileVersion}</p></div>
          <div className="mission-stamp"><Sparkles size={16} /> LIVE BUILD</div>
        </div>
        <div className="readiness-panel">
          <div className="readiness-heading"><div><span>Линия готовности</span><b>{state.profileStatus}</b></div><Gauge size={24} /></div>
          <ol>
            {steps.map((step, index) => (
              <li key={step.label} className={step.done ? "done" : ""}>
                <span className="rail-node">{step.done ? <Check size={14} /> : index + 1}</span>
                <div><small>{step.label}</small><b>{step.value}</b></div>
              </li>
            ))}
          </ol>
          <PlayButton state={state} />
        </div>
      </div>

      {state.downloadPanelVisible && (
        <section className="progress-panel" aria-live="polite">
          <div className="progress-meta"><span>{state.downloadPhase}</span><b>{state.downloadCounter}</b></div>
          <div className="progress-track"><span style={{ width: `${Math.round(state.downloadProgress * 100)}%` }} /></div>
          <div className="progress-file"><Download size={16} /><span>{state.downloadFile}</span></div>
        </section>
      )}

      <div className="dashboard-grid">
        <article className="telemetry-card"><div className="card-kicker"><span>Состояние</span><Gauge size={18} /></div><strong>{state.profileInstalled ? "ГОТОВ" : "ОЖИДАНИЕ"}</strong><p>{state.profileStatus}</p></article>
        <article className="telemetry-card"><div className="card-kicker"><span>Время в игре</span><Gamepad2 size={18} /></div><strong>{state.playtimeTotal}</strong><p>На этом устройстве</p></article>
        <article className="telemetry-card wide"><div className="card-kicker"><span>Последняя сводка</span><Newspaper size={18} /></div><strong>{state.newsItems[0]?.title || "Новостей пока нет"}</strong><p>{state.newsItems[0]?.body || "Когда появится новая сводка проекта, она будет здесь."}</p></article>
      </div>

      <div className={`status-toast${state.message ? " visible" : ""}`} aria-live="polite"><StatusDot active={!state.message.toLowerCase().includes("ошиб")} /><span>{state.message}</span></div>
    </section>
  );
}

function NewsView({ state }: { state: LauncherState }) {
  return (
    <section className="news-view">
      <header className="topbar"><div><span className="eyebrow">Сводки проекта</span><h1>Новости</h1></div><div className="topbar-mark"><MessageCircle size={22} /> Telegram channel</div></header>
      <div className="news-list">
        {state.newsItems.length ? state.newsItems.map((item, index) => (
          <article key={`${item.title}-${index}`} className="news-card">
            <div className="news-index">{String(index + 1).padStart(2, "0")}</div>
            <div><div className="news-meta"><span>{item.date || "Недавно"}</span><span>Project Minecraft</span></div><h2>{item.title || "Сводка проекта"}</h2><p>{item.body}</p></div>
          </article>
        )) : <div className="empty-state"><Newspaper size={38} /><h2>Сводок пока нет</h2><p>Лента обновится автоматически после публикации.</p></div>}
      </div>
    </section>
  );
}

function Toggle({ checked, onChange, label }: { checked: boolean; onChange: (checked: boolean) => void; label: string }) {
  return <button type="button" role="switch" aria-checked={checked} aria-label={label} className={`toggle${checked ? " checked" : ""}`} onClick={() => onChange(!checked)}><span /></button>;
}

function SettingsOverlay({ state }: { state: LauncherState }) {
  if (!state.settingsVisible) return null;
  return (
    <div className="overlay settings-overlay" role="dialog" aria-modal="true" aria-labelledby="settings-title">
      <div className="settings-sheet">
        <header><div><span className="eyebrow">Конфигурация лаунчера</span><h2 id="settings-title">Настройки</h2></div><IconButton label="Закрыть настройки" onClick={() => void action("closeSettings")}><X /></IconButton></header>
        <div className="settings-content">
          <section className="setting-section">
            <div className="setting-heading"><Cpu /><div><h3>Память Minecraft</h3><p>Лаунчер подставит значение в параметры JVM.</p></div><strong>{state.memoryLabel}</strong></div>
            <input className="memory-slider" type="range" min={2} max={state.memoryMax} value={state.memoryGb} onChange={(event) => void action("setMemory", { value: Number(event.target.value) })} aria-label="Объём памяти" />
            <div className="memory-actions"><button onClick={() => void action("memoryDecrease")}>− 1 ГБ</button><button className={state.memoryAuto ? "active" : ""} onClick={() => void action("memoryAuto")}>Автоматически</button><button onClick={() => void action("memoryIncrease")}>+ 1 ГБ</button></div>
          </section>

          {state.discreteGpuAvailable && <section className="setting-row"><div><h3>Дискретная видеокарта</h3><p>{state.discreteGpuLabel}</p></div><Toggle label="Дискретная видеокарта" checked={state.useDiscreteGpu} onChange={(enabled) => void action("setDiscreteGpu", { enabled })} /></section>}
          <section className="setting-row"><div><h3>Статус в Discord</h3><p>Показывать профиль и состояние игры.</p></div><Toggle label="Discord Rich Presence" checked={state.discordRpcEnabled} onChange={(enabled) => void action("setDiscordRpc", { enabled })} /></section>

          <section className="setting-section install-location">
            <div className="setting-heading"><HardDrive /><div><h3>Папка установки</h3><p title={state.installFolder}>{state.installFolder || "Системная папка лаунчера"}</p></div></div>
            <div className="folder-actions"><button onClick={() => void action("openInstallFolder")}><Folder size={18} />Открыть</button><button onClick={() => void action("changeInstallFolder")} disabled={state.isSyncing}><RefreshCw size={18} />Перенести</button></div>
          </section>

          {state.serverVisible && <section className="setting-section"><div className="setting-heading"><Server /><div><h3>Сервер подключения</h3><p>Автовыбор ищет самый быстрый доступный адрес.</p></div></div><select value={state.serverIndex} onChange={(event) => void action("selectServer", { index: Number(event.target.value) })}>{state.serverNames.map((name, index) => <option key={`${name}-${index}`} value={index}>{name}</option>)}</select></section>}
        </div>
      </div>
    </div>
  );
}

function UpdateBanner({ state }: { state: LauncherState }) {
  if (!state.updateReady && !state.updateStatus && !state.updateMandatory) return null;
  return (
    <aside className={`update-banner${state.updateMandatory ? " mandatory" : ""}`}>
      <RefreshCw className={!state.updateReady ? "spin" : ""} />
      <div><b>{state.updateReady ? `Версия ${state.updateVersion} готова` : state.updateStatus || "Проверяем обновление"}</b><span>{state.updateMandatory ? "Обновление обязательно для запуска игры." : "Перезапустите лаунчер, когда будете готовы."}</span></div>
      {state.updateReady && <button onClick={() => void action("restartForUpdate")}>Перезапустить</button>}
    </aside>
  );
}

function PolicyOverlay({ state }: { state: LauncherState }) {
  if (!state.policyVisible) return null;
  return (
    <div className="overlay critical-overlay" role="dialog" aria-modal="true" aria-labelledby="policy-title">
      <section className="policy-card"><div className="policy-icon"><ShieldAlert /></div><span className="eyebrow">Перед запуском игры · {state.policyVersionLabel}</span><h2 id="policy-title">Политика конфиденциальности</h2><div className="policy-text">{state.policyText || "Загружаем актуальный текст политики…"}</div><button className="primary-button" onClick={() => void action("acceptPolicy")} disabled={state.policyAccepting}>{state.policyAccepting ? <RefreshCw className="spin" /> : <Check />} {state.policyAccepting ? "Сохраняем" : "Принять и продолжить"}</button></section>
    </div>
  );
}

function AnticheatOverlay({ message }: { message: string }) {
  if (!message) return null;
  return (
    <div className="overlay critical-overlay" role="alertdialog" aria-modal="true" aria-labelledby="anticheat-title">
      <section className="anticheat-card"><div className="danger-emblem"><ShieldAlert /></div><span className="eyebrow">Защитная система</span><h2 id="anticheat-title">Запуск остановлен</h2><p>{message}</p><button className="danger-button" onClick={() => void action("dismissAnticheatAlert")}><X />Закрыть уведомление</button></section>
    </div>
  );
}

export default function App() {
  const state = useLauncherState();
  const [tab, setTab] = useState<"home" | "news">("home");

  return (
    <div className="app-shell">
      <div className="ambient" aria-hidden="true" />
      {state.sessionRestoring ? <RestoreScreen state={state} /> : !state.isAuthenticated ? <LoginScreen state={state} /> : (
        <div className="authenticated-layout">
          <Sidebar state={state} tab={tab} setTab={setTab} />
          <main className="workspace">{tab === "home" ? <HomeView state={state} /> : <NewsView state={state} />}</main>
        </div>
      )}
      <UpdateBanner state={state} />
      <SettingsOverlay state={state} />
      <PolicyOverlay state={state} />
      <AnticheatOverlay message={state.anticheatAlert} />
    </div>
  );
}
