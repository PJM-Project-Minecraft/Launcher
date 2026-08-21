import { FormEvent, useEffect, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { getCurrentWindow } from "@tauri-apps/api/window";

const logoAsset = new URL("../src-tauri/assets/logo.svg", import.meta.url).href;

type NewsItem = { title: string; date: string; body: string };
type DeliveryState = { phase: string; message: string; version: string; progress: number; mandatory: boolean; retryable: boolean };
type InstallMigrationState = { phase: string; source: string; destination: string; error: string; retryable: boolean };

type LauncherState = {
	launcherDelivery: DeliveryState;
	profileDelivery: DeliveryState;
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
  installMigration: InstallMigrationState;
  newsItems: NewsItem[];
  anticheatAlert: string;
  policyVisible: boolean;
  policyAccepting: boolean;
  policyText: string;
  policyVersionLabel: string;
  policyVersion: number;
};

const emptyState: LauncherState = {
	launcherDelivery: { phase: "idle", message: "", version: "", progress: 0, mandatory: false, retryable: false },
	profileDelivery: { phase: "checking", message: "Проверяем файлы", version: "", progress: 0, mandatory: false, retryable: false },
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
  installMigration: { phase: "", source: "", destination: "", error: "", retryable: false },
  newsItems: [],
  anticheatAlert: "",
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
  if (screen === "download") {
    return { ...browserPreview, isSyncing: true, downloadPanelVisible: true, downloadPhase: "Синхронизация файлов", downloadCounter: "142 / 318", downloadFile: "mods/project-minecraft-content.jar", downloadProgress: .45 };
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

function WindowTitleBar() {
  const windowAction = (run: (window: ReturnType<typeof getCurrentWindow>) => Promise<void>) => {
    if (isTauriRuntime()) void run(getCurrentWindow());
  };

  return (
    <header className="window-titlebar">
      <div
        className="window-drag-region"
        data-tauri-drag-region
        onDoubleClick={() => windowAction((window) => window.toggleMaximize())}
      >
        <strong>Project Minecraft</strong>
        <span>Launcher</span>
      </div>
      <div className="window-controls">
        <button aria-label="Свернуть" title="Свернуть" onClick={() => windowAction((window) => window.minimize())}><i className="window-glyph minimize" /></button>
        <button aria-label="Развернуть" title="Развернуть" onClick={() => windowAction((window) => window.toggleMaximize())}><i className="window-glyph maximize" /></button>
        <button className="window-close" aria-label="Закрыть" title="Закрыть" onClick={() => windowAction((window) => window.close())}><PixelIcon name="close" /></button>
      </div>
    </header>
  );
}

function StatusDot({ active = true }: { active?: boolean }) {
  return <span className={`status-dot${active ? " active" : ""}`} aria-hidden="true" />;
}

type PixelIconName = "article" | "check" | "close" | "corner" | "cpu" | "download" | "folder" | "gamepad" | "lock" | "logout" | "play" | "reload" | "server" | "user";

function PixelIcon({ name, className = "" }: { name: PixelIconName; className?: string }) {
  return <span className={`pixel-icon pixel-icon-${name}${className ? ` ${className}` : ""}`} aria-hidden="true" />;
}

function Brand({ compact = false }: { compact?: boolean }) {
  return <div className={`brand${compact ? " compact" : ""}`}><img className="brand-logo" src={logoAsset} alt="Project Minecraft" /></div>;
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
    <main className="auth-screen">
      <header className="auth-header"><Brand /></header>
      <form className="login-card" onSubmit={submit}>
        <div className="card-kicker"><span>Вход в аккаунт</span><PixelIcon name="lock" /></div>
        <h1>Авторизация</h1>

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
          <PixelIcon name={state.isLoading ? "reload" : "corner"} className={state.isLoading ? "spin" : ""} />
          {state.isLoading ? "Проверяем аккаунт" : "Войти"}
        </button>
      </form>
    </main>
  );
}

function RestoreScreen({ state }: { state: LauncherState }) {
  return (
    <main className="restore-layout" aria-live="polite">
      <header className="auth-header"><Brand /></header>
      <section>
        <PixelIcon name="reload" className="restore-spinner spin" />
        <span className="eyebrow">Подключение</span>
        <h1>Возвращаем сессию</h1>
        <p>{state.message}</p>
      </section>
    </main>
  );
}

function HeaderBar({ state, tab, setTab }: { state: LauncherState; tab: "home" | "news"; setTab: (tab: "home" | "news") => void }) {
  return (
    <header className="launcher-header">
      <div className="header-inner">
        <Brand compact />
        <nav aria-label="Основная навигация">
          <button className={tab === "home" ? "active" : ""} onClick={() => setTab("home")}>Игра</button>
          <button className={tab === "news" ? "active" : ""} onClick={() => setTab("news")}>Новости</button>
          <button onClick={() => void action("openSettings")}>Настройки</button>
          <button onClick={() => void action("openUrl", { url: "https://pjm.likonchik.xyz" })}>Сайт</button>
          <button onClick={() => void action("openUrl", { url: "https://t.me/project_minecraft" })}>Поддержка</button>
        </nav>
        <div className="account-block">
          <PixelIcon name="user" />
          <div><b>{state.userLogin}</b><span><StatusDot /> Онлайн</span></div>
          <IconButton label="Выйти из аккаунта" danger onClick={() => void action("logout")}><PixelIcon name="logout" /></IconButton>
        </div>
      </div>
    </header>
  );
}

function PlayButton({ state }: { state: LauncherState }) {
  const label = state.launcherDelivery.mandatory
    ? "Требуется обновление"
    : state.profileDelivery.phase === "syncing"
      ? state.downloadPhase || "Подготовка"
      : state.profileDelivery.phase === "checking"
        ? "Проверяем файлы"
        : state.profileDelivery.phase === "failed"
          ? "Проверить сборку"
          : state.profileDelivery.phase === "missing"
            ? "Установить сборку"
            : state.profileDelivery.phase === "updateAvailable"
              ? "Обновить сборку"
              : "Играть";

  return (
    <button className="launch-button" onClick={() => void action("play")} disabled={!state.hasProfile || state.isSyncing || Boolean(state.installMigration.phase) || state.profileDelivery.phase === "syncing" || state.launcherDelivery.mandatory}>
      <PixelIcon name={state.profileDelivery.phase === "syncing" || state.profileDelivery.phase === "checking" ? "reload" : state.profileDelivery.phase === "current" ? "play" : "download"} className={state.profileDelivery.phase === "syncing" || state.profileDelivery.phase === "checking" ? "spin" : ""} />
      <span>{label}</span>
    </button>
  );
}

function launchStatus(state: LauncherState) {
  if (state.launcherDelivery.mandatory) return "Обновите лаунчер для продолжения";
  if (state.profileDelivery.phase === "syncing") return state.profileDelivery.message || "Подготавливаем файлы";
  if (state.profileDelivery.phase === "checking") return "Проверяем файлы";
  if (state.profileDelivery.phase === "failed") return state.profileDelivery.message || "Требуется проверка файлов";
  if (!state.hasProfile) return "Игра сейчас недоступна";
  if (state.profileDelivery.phase === "missing") return "Игра не установлена";
  if (state.profileDelivery.phase === "updateAvailable") return "Доступно обновление";
  return "Готово к запуску";
}

function HomeView({ state }: { state: LauncherState }) {
  return (
    <section className="home-view">
      <div className="utility-row">
        <button className="quick-settings" onClick={() => void action("openSettings")}><PixelIcon name="cpu" /> Настройки</button>
      </div>

      <div className={`launch-console${state.downloadPanelVisible ? " downloading" : ""}`}>
        <div className="launch-readout" aria-live="polite">
          <span>{state.isSyncing ? state.downloadCounter || "Синхронизация" : "Состояние игры"}</span>
          <strong>{launchStatus(state)}</strong>
          {state.downloadPanelVisible && <small>{state.downloadFile}</small>}
        </div>
        <PlayButton state={state} />
        {state.downloadPanelVisible && <div className="launch-progress" style={{ "--launch-progress": `${Math.round(state.downloadProgress * 100)}%` } as CSSProperties}><span /></div>}
      </div>
    </section>
  );
}

function NewsView({ state }: { state: LauncherState }) {
  return (
    <section className="news-view">
      <header className="view-title"><span>Project Minecraft</span><h1>Новости</h1></header>
      <div className="news-list">
        {state.newsItems.length ? state.newsItems.map((item, index) => (
          <article key={`${item.title}-${index}`} className="news-card">
            <div className="news-meta"><span>{item.date || "Недавно"}</span><span>Project Minecraft</span></div><h2>{item.title || "Сводка проекта"}</h2><p>{item.body}</p>
          </article>
        )) : <div className="empty-state"><PixelIcon name="article" /><h2>Сводок пока нет</h2><p>Лента обновится автоматически после публикации.</p></div>}
      </div>
    </section>
  );
}

function Toggle({ checked, onChange, label }: { checked: boolean; onChange: (checked: boolean) => void; label: string }) {
  return <button type="button" role="switch" aria-checked={checked} aria-label={label} className={`pixel-toggle${checked ? " checked" : ""}`} onClick={() => onChange(!checked)}><span /><b>{checked ? "ВКЛ" : "ВЫКЛ"}</b></button>;
}

function SettingsOverlay({ state }: { state: LauncherState }) {
  const [present, setPresent] = useState(state.settingsVisible);

  useEffect(() => {
    if (state.settingsVisible) setPresent(true);
  }, [state.settingsVisible]);

  if (!state.settingsVisible && !present) return null;
  const memoryProgress = Math.max(0, Math.min(100, ((state.memoryGb - 2) / Math.max(1, state.memoryMax - 2)) * 100));
  const closing = !state.settingsVisible;
  return (
    <div
      className={`overlay settings-overlay${closing ? " closing" : ""}`}
      role="dialog"
      aria-modal="true"
      aria-labelledby="settings-title"
      onClick={() => void action("closeSettings")}
      onAnimationEnd={(event) => {
        if (closing && event.target === event.currentTarget) setPresent(false);
      }}
    >
      <div className="settings-sheet" onClick={(event) => event.stopPropagation()}>
        <header className="settings-header"><div><span>Параметры клиента</span><h2 id="settings-title">Настройки</h2></div><IconButton label="Закрыть настройки" onClick={() => void action("closeSettings")}><PixelIcon name="close" /></IconButton></header>
        <div className="settings-layout">
          <aside className="settings-memory-panel">
            <div className="memory-emblem"><PixelIcon name="cpu" /></div>
            <span className="settings-caption">Оперативная память</span>
            <strong className="memory-number">{state.memoryGb}<small>ГБ</small></strong>
            <div className="memory-control" style={{ "--memory-progress": `${memoryProgress}%` } as CSSProperties}>
              <span className="memory-fill" />
              <input className="memory-slider" type="range" min={2} max={state.memoryMax} value={state.memoryGb} onChange={(event) => void action("setMemory", { value: Number(event.target.value) })} aria-label="Объём памяти" />
            </div>
            <div className="memory-actions"><button onClick={() => void action("memoryDecrease")}>− 1</button><button className={state.memoryAuto ? "active" : ""} onClick={() => void action("memoryAuto")}>Авто</button><button onClick={() => void action("memoryIncrease")}>+ 1</button></div>
          </aside>

          <div className="settings-main">
            <section className="settings-group">
              <header className="settings-group-title"><PixelIcon name="gamepad" /><div><h3>Система</h3><p>Производительность и присутствие в игре.</p></div></header>
              {state.discreteGpuAvailable && <div className="settings-option"><div className="settings-option-copy"><strong>Дискретная видеокарта</strong><span>{state.discreteGpuLabel}</span></div><Toggle label="Дискретная видеокарта" checked={state.useDiscreteGpu} onChange={(enabled) => void action("setDiscreteGpu", { enabled })} /></div>}
              <div className="settings-option"><div className="settings-option-copy"><strong>Статус в Discord</strong><span>Показывать профиль и состояние игры.</span></div><Toggle label="Discord Rich Presence" checked={state.discordRpcEnabled} onChange={(enabled) => void action("setDiscordRpc", { enabled })} /></div>
            </section>

            <section className="settings-group">
              <header className="settings-group-title"><PixelIcon name="folder" /><div><h3>Файлы</h3><p>Расположение установленного клиента.</p></div></header>
              <div className="settings-option settings-folder-option"><div className="settings-option-copy"><strong>Папка установки</strong><span title={state.installFolder}>{state.installFolder || "Системная папка лаунчера"}</span></div><div className="folder-actions"><button onClick={() => void action("openInstallFolder")}><PixelIcon name="folder" />Открыть</button><button onClick={() => void action("changeInstallFolder")} disabled={state.isSyncing}><PixelIcon name="reload" />Перенести</button></div></div>
              {state.installMigration.phase && <div className="settings-option"><div className="settings-option-copy"><strong>{state.installMigration.phase === "failed" ? "Перенос остановлен" : state.installMigration.phase === "running" ? "Перенос выполняется" : "Перенос ожидает продолжения"}</strong><span>{state.installMigration.error || `${state.installMigration.source} → ${state.installMigration.destination}`}</span></div>{state.installMigration.retryable && <button onClick={() => void action("retryInstallMigration")} disabled={state.isSyncing}><PixelIcon name="reload" />Повторить</button>}</div>}
            </section>

            {state.serverVisible && <section className="settings-group"><header className="settings-group-title"><PixelIcon name="server" /><div><h3>Подключение</h3><p>Адрес для авторизации и загрузок.</p></div></header><div className="settings-option settings-server-option"><div className="settings-option-copy"><strong>Сервер</strong><span>Автовыбор использует самый быстрый доступный адрес.</span></div><div className="select-field"><select value={state.serverIndex} onChange={(event) => void action("selectServer", { index: Number(event.target.value) })}>{state.serverNames.map((name, index) => <option key={`${name}-${index}`} value={index}>{name}</option>)}</select><PixelIcon name="corner" /></div></div></section>}
          </div>
        </div>
      </div>
    </div>
  );
}

function UpdateBanner({ state }: { state: LauncherState }) {
  const update = state.launcherDelivery;
  if (update.phase === "idle" || update.phase === "current") return null;
  const ready = update.phase === "ready";
  return (
    <aside className={`update-banner${update.mandatory ? " mandatory" : ""}`}>
      <PixelIcon name="reload" className={update.phase === "downloading" || update.phase === "applying" ? "spin" : ""} />
      <div><b>{ready ? `Версия ${update.version} готова` : update.message || "Проверяем обновление"}</b><span>{update.mandatory ? "Обновление обязательно для запуска игры." : ready ? update.message || "Перезапустите лаунчер, когда будете готовы." : update.phase === "failed" ? "Можно повторить без перезапуска лаунчера." : `Загрузка ${Math.round(update.progress * 100)}%`}</span></div>
      {ready && <button onClick={() => void action("restartForUpdate")}>Перезапустить</button>}
      {update.phase === "failed" && update.retryable && <button onClick={() => void action("retryUpdate")}>Повторить</button>}
    </aside>
  );
}

function PolicyOverlay({ state }: { state: LauncherState }) {
  if (!state.policyVisible) return null;
  return (
    <div className="overlay critical-overlay" role="dialog" aria-modal="true" aria-labelledby="policy-title">
      <section className="policy-card"><div className="policy-icon"><PixelIcon name="lock" /></div><span className="eyebrow">Перед запуском игры · {state.policyVersionLabel}</span><h2 id="policy-title">Политика конфиденциальности</h2><div className="policy-text">{state.policyText || "Загружаем актуальный текст политики…"}</div><button className="primary-button" onClick={() => void action("acceptPolicy")} disabled={state.policyAccepting}><PixelIcon name={state.policyAccepting ? "reload" : "check"} className={state.policyAccepting ? "spin" : ""} /> {state.policyAccepting ? "Сохраняем" : "Принять и продолжить"}</button></section>
    </div>
  );
}

function AnticheatOverlay({ message }: { message: string }) {
  if (!message) return null;
  return (
    <div className="overlay critical-overlay" role="alertdialog" aria-modal="true" aria-labelledby="anticheat-title">
      <section className="anticheat-card"><div className="danger-emblem"><PixelIcon name="lock" /></div><span className="eyebrow">Защитная система</span><h2 id="anticheat-title">Запуск остановлен</h2><p>{message}</p><button className="danger-button" onClick={() => void action("dismissAnticheatAlert")}><PixelIcon name="close" />Закрыть уведомление</button></section>
    </div>
  );
}

export default function App() {
  const state = useLauncherState();
  const [tab, setTab] = useState<"home" | "news">("home");

  return (
    <div className="app-shell">
      <WindowTitleBar />
      <div className="app-viewport">
        <div className="ambient" aria-hidden="true" />
        {state.sessionRestoring ? <RestoreScreen state={state} /> : !state.isAuthenticated ? <LoginScreen state={state} /> : (
          <div className="authenticated-layout">
            <HeaderBar state={state} tab={tab} setTab={setTab} />
            <main className="workspace"><div key={tab} className={`screen-view screen-${tab}`}>{tab === "home" ? <HomeView state={state} /> : <NewsView state={state} />}</div></main>
          </div>
        )}
        <UpdateBanner state={state} />
        <SettingsOverlay state={state} />
        <PolicyOverlay state={state} />
        <AnticheatOverlay message={state.anticheatAlert} />
      </div>
    </div>
  );
}
