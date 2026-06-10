# Dashboard Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Полный редизайн админ-панели по спеке `docs/superpowers/specs/2026-06-10-dashboard-redesign-design.md`: монохромная стеклянная тема, hover-сайдбар, тосты/модалки/скелетоны/Ctrl+K/SSE, разбиение монолитов.

**Architecture:** Foundation-first: сначала дизайн-токены (Tailwind 4) и общая инфраструктура (`lib/api.ts`, ui-примитивы, toast/confirm, shell), затем поочерёдный порт страниц со старых компонентов на новые. Старые файлы в `app/ui/` живут до окончания порта своей страницы, удаляются в финальной чистке. Backend API не меняется.

**Tech Stack:** Next.js 15 (App Router), React 19, Tailwind CSS 4 (`@tailwindcss/postcss`), framer-motion 12, lucide-react.

**Проверка каждой задачи:** фронтовых тестов в проекте нет; критерий — `npm run build` (включает tsc) без ошибок + дев-прогон в браузере на финальной задаче. Build гонять из `dashboard/`.

**Инвентарь API (обязан сохраниться после порта):**

| Страница | Эндпоинты |
|---|---|
| login | POST `/api/auth/login`; GET `/api/auth/me` |
| обзор | GET `/api/admin/stats`, GET `/health`, SSE `/api/profiles/events` |
| users | GET `/api/admin/users?q=`, PATCH `/api/admin/users/:id/role`, POST `/api/admin/users/:id/{ban,unban}`, POST `/api/admin/users/:id/{hwid-ban,hwid-unban}` (точные сегменты взять из текущего `users-admin.tsx`), DELETE `/api/admin/users/:id` |
| profiles | GET/POST `/api/admin/profiles`, PATCH/DELETE `/api/admin/profiles/:id`, POST `/api/admin/profiles/:id/scan`, POST `/api/admin/profiles/:id/prepare-client`, GET `/api/admin/profiles/loader-options?gameVersion=` |
| anticheat | GET `/api/admin/anticheat/detections?limit=200`, GET/POST/DELETE `/api/admin/anticheat/bans/{account,hwid}(/:key)`, GET/POST/DELETE `/api/admin/anticheat/signatures(/:id)` |

---

### Task 1: Tailwind 4 + дизайн-токены + layout

**Files:**
- Modify: `dashboard/package.json` (deps)
- Create: `dashboard/postcss.config.mjs`
- Rewrite: `dashboard/app/globals.css`
- Modify: `dashboard/app/layout.tsx`

- [ ] **Step 1:** `cd dashboard && npm install tailwindcss @tailwindcss/postcss`
- [ ] **Step 2:** `postcss.config.mjs`:

```js
export default { plugins: { '@tailwindcss/postcss': {} } };
```

- [ ] **Step 3:** Новый `app/globals.css` (старые 886 строк удаляются целиком):

```css
@import "tailwindcss";

@theme {
  --color-bg: #070709;
  --color-surface: rgba(255, 255, 255, 0.04);
  --color-surface-strong: rgba(255, 255, 255, 0.07);
  --color-edge: rgba(255, 255, 255, 0.08);
  --color-edge-strong: rgba(255, 255, 255, 0.14);
  --color-fg: #fafafa;
  --color-fg-secondary: #a1a1aa;
  --color-fg-muted: #8b8b94;
  --color-fg-faint: #5f5f66;
  --color-danger: #f87171;
  --color-warn: #fbbf24;
  --color-ok: #a1e3b1;
  --font-sans: "Inter", system-ui, sans-serif;
}

html, body { background: var(--color-bg); color: var(--color-fg); }
```

Утилита для стеклянных поверхностей — компонент Card (Task 3), отдельные css-классы не плодим.

- [ ] **Step 4:** В `layout.tsx`: удалить link на Material Symbols, оставить Inter; `<body className="antialiased">`.
- [ ] **Step 5:** Временная совместимость: старые страницы ссылаются на классы старого CSS — после замены globals.css они выглядят сломанно, но **build обязан проходить**. Проверить: `npm run build`.
- [ ] **Step 6:** Commit: `feat(dashboard): tailwind 4 + дизайн-токены`

### Task 2: lib/types.ts + lib/api.ts + lib/use-sse.ts

**Files:**
- Create: `dashboard/app/lib/types.ts` — перенести и объединить типы из `ui/profile-admin.tsx`, `ui/users-admin.tsx`, `ui/anticheat-admin.tsx`, `ui/auth.ts` (Profile, ProfileFile, LoaderOptions, AdminUser, UserDetail, AuthLogEntry, Detection, AccountBan, HwidBan, CheatSignature, Stats, AuthUser). Каждый тип — точная копия полей из старых файлов (читать их при выполнении).
- Create: `dashboard/app/lib/api.ts`:

```ts
export const apiUrl = (process.env.NEXT_PUBLIC_API_URL ?? 'http://127.0.0.1:8080').replace(/\/$/, '');
export const tokenKey = 'launcher.admin.token';

export class ApiError extends Error {
  constructor(message: string, readonly status: number) { super(message); }
}

export function getToken(): string | null {
  if (typeof window === 'undefined') return null;
  return window.localStorage.getItem(tokenKey);
}
export function setToken(token: string) { window.localStorage.setItem(tokenKey, token); }
export function clearToken() { window.localStorage.removeItem(tokenKey); }

type Options = { method?: string; body?: unknown; auth?: boolean };

export async function api<T = unknown>(path: string, options: Options = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (options.auth !== false) {
    const token = getToken();
    if (token) headers.Authorization = `Bearer ${token}`;
  }
  let response: Response;
  try {
    response = await fetch(`${apiUrl}${path}`, {
      method: options.method ?? 'GET',
      headers,
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    });
  } catch {
    throw new ApiError('Backend недоступен', 0);
  }
  if (response.status === 401 && options.auth !== false) {
    clearToken();
    if (typeof window !== 'undefined') window.location.href = '/login';
    throw new ApiError('Сессия истекла', 401);
  }
  if (!response.ok) {
    let message = `Ошибка ${response.status}`;
    try {
      const data = await response.json();
      if (data?.message) message = data.message;
    } catch { /* тело не JSON */ }
    throw new ApiError(message, response.status);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}
```

- Create: `dashboard/app/lib/use-sse.ts`:

```ts
'use client';
import { useEffect, useRef } from 'react';
import { apiUrl } from './api';

/** Подписка на SSE-события профилей; onEvent зовётся на каждое сообщение.
 *  Реконнект встроен в EventSource; при размонтировании соединение закрывается. */
export function useProfileEvents(onEvent: () => void) {
  const handler = useRef(onEvent);
  handler.current = onEvent;
  useEffect(() => {
    const source = new EventSource(`${apiUrl}/api/profiles/events`);
    source.onmessage = () => handler.current();
    return () => source.close();
  }, []);
}
```

- [ ] **Step 1:** Создать три файла (типы — переносом из старых компонентов).
- [ ] **Step 2:** `npm run build` → PASS. Commit: `feat(dashboard): единый api-клиент, общие типы, sse-хук`

### Task 3: UI-примитивы

**Files:** Create `dashboard/components/ui/{button,card,stat-card,badge,input,field,tabs,table,skeleton,empty-state,spinner}.tsx`

Все — client-компоненты с `className`-passthrough (конкатенация строк, без clsx). Базовые стили:

- **Button** (`variant: 'primary' | 'ghost' | 'danger'`, `loading?`): primary — `bg-fg text-bg hover:opacity-90`; ghost — `border border-edge bg-surface hover:bg-surface-strong`; danger — `border border-danger/40 text-danger hover:bg-danger/10`; все — `rounded-lg px-4 h-10 text-sm font-semibold transition disabled:opacity-50`, при `loading` — `<Spinner size={14}/>` слева и disabled.
- **Card**: `rounded-xl border border-edge bg-surface backdrop-blur-xl p-5`.
- **StatCard** (`label, value, tone?: 'default'|'warn'|'danger'|'ok', hint?`): Card с label `text-xs uppercase tracking-wide text-fg-muted`, value `text-2xl font-bold`, tone красит value.
- **Badge** (`tone: 'default'|'ok'|'warn'|'danger'`): `inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-xs border`, тон — цвет текста/границы.
- **Input/Select/TextArea**: `w-full rounded-lg border border-edge bg-surface px-3 h-10 text-sm placeholder:text-fg-faint focus:border-edge-strong focus:outline-none` (TextArea — `min-h-24 py-2`).
- **Field** (`label, children, hint?`): label `text-xs font-semibold uppercase tracking-wide text-fg-muted mb-1.5`.
- **Tabs** (`items: {key,label,badge?}[], active, onChange`): строка кнопок, активная — белая плашка (motion layoutId="tab").
- **Table**: `<Table>` оборачивает `overflow-x-auto` + `<table class="w-full text-sm">`; `<Th>` `text-left text-xs uppercase text-fg-faint font-medium px-3 py-2`; `<Td>` `px-3 py-2.5 border-t border-edge/60`. Кликабельные строки — `hover:bg-surface cursor-pointer`.
- **Skeleton** (`className`): `animate-pulse rounded-md bg-surface-strong`; `SkeletonTable rows cols` — готовая сетка.
- **EmptyState** (`icon?: LucideIcon, title, hint?, action?`): по центру, приглушённо.
- **Spinner** (`size?`): lucide `Loader2` c `animate-spin`.

- [ ] **Step 1:** Создать файлы. **Step 2:** `npm run build` → PASS. **Step 3:** Commit: `feat(dashboard): ui-примитивы`

### Task 4: Тосты

**Files:** Create `dashboard/components/ui/toast.tsx`

```tsx
'use client';
import { createContext, useCallback, useContext, useRef, useState } from 'react';
import { AnimatePresence, motion } from 'framer-motion';
import { CheckCircle2, AlertCircle, Info } from 'lucide-react';

type ToastKind = 'success' | 'error' | 'info';
type Toast = { id: number; kind: ToastKind; text: string };
const ToastContext = createContext<(kind: ToastKind, text: string) => void>(() => {});

export function useToast() { return useContext(ToastContext); }

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextId = useRef(1);
  const push = useCallback((kind: ToastKind, text: string) => {
    const id = nextId.current++;
    setToasts((list) => [...list, { id, kind, text }]);
    setTimeout(() => setToasts((list) => list.filter((t) => t.id !== id)), 4000);
  }, []);
  const icons = { success: CheckCircle2, error: AlertCircle, info: Info };
  const tones = { success: 'text-ok', error: 'text-danger', info: 'text-fg-secondary' };
  return (
    <ToastContext.Provider value={push}>
      {children}
      <div className="fixed bottom-5 right-5 z-50 flex flex-col gap-2">
        <AnimatePresence>
          {toasts.map((toast) => {
            const Icon = icons[toast.kind];
            return (
              <motion.div key={toast.id}
                initial={{ opacity: 0, x: 24 }} animate={{ opacity: 1, x: 0 }} exit={{ opacity: 0, x: 24 }}
                className="flex items-center gap-2.5 rounded-lg border border-edge bg-bg/90 backdrop-blur-xl px-4 py-3 text-sm shadow-xl">
                <Icon size={16} className={tones[toast.kind]} />
                {toast.text}
              </motion.div>
            );
          })}
        </AnimatePresence>
      </div>
    </ToastContext.Provider>
  );
}
```

- [ ] Build PASS → Commit: `feat(dashboard): тосты`

### Task 5: Modal + ConfirmDialog

**Files:** Create `dashboard/components/ui/modal.tsx`, `dashboard/components/ui/confirm.tsx`

- **Modal** (`open, onClose, title, children, footer?`): фикс-оверлей `bg-black/60 backdrop-blur-sm`, по центру Card `max-w-md w-full`, AnimatePresence (scale 0.96→1), Esc и клик по фону закрывают.
- **Confirm** — promise-based:

```tsx
'use client';
import { createContext, useContext, useRef, useState } from 'react';
import { Modal } from './modal';
import { Button } from './button';

type ConfirmOptions = { title: string; message: string; confirmLabel?: string; danger?: boolean };
const ConfirmContext = createContext<(opts: ConfirmOptions) => Promise<boolean>>(async () => false);
export function useConfirm() { return useContext(ConfirmContext); }

export function ConfirmProvider({ children }: { children: React.ReactNode }) {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const resolver = useRef<(ok: boolean) => void>(null);
  const confirm = (options: ConfirmOptions) =>
    new Promise<boolean>((resolve) => { resolver.current = resolve; setOpts(options); });
  const finish = (ok: boolean) => { resolver.current?.(ok); setOpts(null); };
  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <Modal open={opts !== null} onClose={() => finish(false)} title={opts?.title ?? ''}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onClick={() => finish(false)}>Отмена</Button>
            <Button variant={opts?.danger ? 'danger' : 'primary'} onClick={() => finish(true)}>
              {opts?.confirmLabel ?? 'Подтвердить'}
            </Button>
          </div>
        }>
        <p className="text-sm text-fg-secondary">{opts?.message}</p>
      </Modal>
    </ConfirmContext.Provider>
  );
}
```

- [ ] Build PASS → Commit: `feat(dashboard): модалки и confirm-диалог`

### Task 6: Shell — Sidebar (hover), Topbar, AppFrame

**Files:** Create `dashboard/components/shell/{sidebar,topbar,app-frame}.tsx`; Modify `dashboard/app/layout.tsx` (заменить импорт AppFrame); Delete (в Task 13): `app/ui/app-frame.tsx`, `app/ui/sidebar.tsx`.

- **Sidebar**: `nav` фиксированной ширины 64px (`w-16`), элементы — иконки lucide (LayoutDashboard, Package, Users, Newspaper, Shield, LogOut). `onMouseEnter` → состояние `expanded`, motion.nav `animate={{ width: expanded ? 240 : 64 }}` (spring, ~200ms), `position: fixed; inset-y-0 left-0; z-40`, при expanded — `shadow-2xl` и фон `bg-bg/90 backdrop-blur-2xl`; подписи пунктов — `motion.span` c opacity. Контент страницы имеет постоянный `pl-16` — раскрытие НЕ сдвигает контент (оверлей). Активный пункт — белая плашка (`layoutId="nav"`). Затемнение контента при expanded: полупрозрачный фикс-оверлей `bg-black/30 z-30` (pointer-events-none).
- **Topbar**: `sticky top-0 z-20 h-14 border-b border-edge bg-bg/80 backdrop-blur-xl flex items-center px-5 gap-4`; слева — `usePathname()` → заголовок раздела; справа — кнопка поиска (`Search` icon + «Ctrl K», открывает палитру из Task 7), статус API (poll `/health` каждые 30с: точка `bg-ok`/`bg-danger` + текст), инициал админа (из `/api/auth/me`, кэш в состоянии AppFrame), меню выхода не нужно — выход в сайдбаре. На <820px — бургер, открывающий сайдбар оверлеем (state,`expanded` принудительно).
- **AppFrame**: прежний guard из `app/ui/app-frame.tsx` (токен → `/api/auth/me` → role==='admin' или редирект `/login`), плюс обёртки `<ToastProvider><ConfirmProvider>`; для `/login` — рендер без shell.

- [ ] **Step 1:** Реализовать, переключить `layout.tsx` на новый AppFrame.
- [ ] **Step 2:** Build PASS; `npm run dev` — залогиниться, увидеть рейку, наведение раскрывает, уход мыши сворачивает.
- [ ] **Step 3:** Commit: `feat(dashboard): shell — hover-сайдбар, топбар, guard`

### Task 7: CommandPalette (Ctrl+K)

**Files:** Create `dashboard/components/shell/command-palette.tsx`; Modify `topbar.tsx` (открытие).

Поведение: глобальный keydown (Ctrl+K / Cmd+K) и кнопка в топбаре; Modal-подобный оверлей сверху по центру (`max-w-lg`), Input автофокус. Источники: (1) статические команды «Перейти: Обзор/Профили/Пользователи/Новости/Античит» — фильтр по подстроке; (2) если query ≥ 2 символов — debounce 250мс, GET `/api/admin/users?q=` → пункты «Игрок: <login>» (переход на `/users?q=<login>`). Стрелки/Enter/Esc; выбранный пункт — `bg-surface-strong`. На странице Users прочитать `useSearchParams()` q как начальный фильтр.

- [ ] Build PASS; dev-проверка Ctrl+K → Commit: `feat(dashboard): командная палитра`

### Task 8: Login

**Files:** Rewrite `dashboard/app/login/page.tsx` (логика — из текущего файла: POST `/api/auth/login` {login,password,totp?}, ответ с токеном → setToken → `/`; ошибка 401 с requires_two_factor → показать TOTP-поле).

Вид: центр экрана, Card `max-w-sm w-full`, заголовок «PJM Admin», Field+Input (логин/пароль/TOTP при необходимости), Button primary full-width c loading, ошибка — тост + красная рамка полей. motion fade-in.

- [ ] Build PASS; dev: неверный пароль → тост ошибки; верный → редирект. Commit: `feat(dashboard): страница входа`

### Task 9: Обзор

**Files:** Rewrite `dashboard/app/page.tsx`; Create `dashboard/components/overview/{stats-row,events-feed}.tsx`; Delete (Task 13): `app/ui/api-health.tsx`.

- `GET /api/admin/stats` → StatCard×4: игроки онлайн, аккаунты, детекты 24ч (tone warn при >0), статус API (из `/health`, tone ok/danger). Поля Stats взять из текущего использования `/api/admin/stats` (прочитать обработчик в `backend/internal/adminapi` при неясности).
- Лента событий: если в stats есть массив последних событий — использовать его; иначе собрать из `GET /api/admin/anticheat/detections?limit=5` + последние входы из user detail API недоступны списком — тогда лента = детекты + обновления профилей (момент SSE-события, локально). Не изобретать новые backend-эндпоинты.
- SSE `useProfileEvents` → refetch stats; скелетоны при первой загрузке.

- [ ] Build PASS; dev-проверка → Commit: `feat(dashboard): обзор`

### Task 10: Пользователи

**Files:** Rewrite `dashboard/app/users/page.tsx`; Create `dashboard/components/users/{users-table,user-detail-panel}.tsx`; Delete (Task 13): `app/ui/users-admin.tsx`.

Порт `users-admin.tsx` (298 строк) 1:1 по функциям: поиск (`?q=`), таблица (Table-примитив: логин, email, роль, Badge-флаги бан/HWID/2FA, последний вход), клик → панель справа (motion slide-in, ширина ~380px): детали, AuthLog-список, действия: смена роли (Select+PATCH), бан/разбан, HWID-бан/разбан, удалить. Все мутации: confirm (danger для удаления/бана) → api() → toast → refetch. Скелетон-таблица при загрузке, EmptyState при пустом поиске. Начальный q — из useSearchParams (палитра).

- [ ] Build PASS; dev: бан/разбан с confirm и тостом → Commit: `feat(dashboard): пользователи`

### Task 11: Античит

**Files:** Rewrite `dashboard/app/anticheat/page.tsx`; Create `dashboard/components/anticheat/{detections-tab,bans-tab,signatures-tab}.tsx`; Delete (Task 13): `app/ui/anticheat-admin.tsx`.

Порт `anticheat-admin.tsx` (323 строки): Tabs (Детекты / Баны / Сигнатуры). Детекты: таблица, severity-Badge (≥8 danger, ≥5 warn), выпуск бана из строки (confirm). Баны: два списка (account/hwid) с удалением (confirm) и формой добавления. Сигнатуры: список + форма создания + удаление. Статкарты сверху (кол-во детектов/банов) — StatCard.

- [ ] Build PASS; dev-проверка табов → Commit: `feat(dashboard): античит`

### Task 12: Профили (порт монолита)

**Files:** Rewrite `dashboard/app/profiles/page.tsx`; Create `dashboard/components/profiles/{profile-list,profile-editor,client-flow}.tsx`; Delete (Task 13): `app/ui/profile-admin.tsx`.

Порт `profile-admin.tsx` (1053 строки) с сохранением всей логики (перед портом прочитать файл целиком; вся работа с scan/prepare/manifest, loader-options, статусы шагов — копируется, меняется только разметка/стили):
- **profile-list**: карточки профилей (имя, версия, лоадер, статус-Badge, кол-во файлов), «+ Новый профиль», клик → выбор.
- **profile-editor**: форма (двухколоночный grid из Field/Input/Select/TextArea), loader-options подгрузка по версии, сохранение (POST/PATCH) с toast, удаление с confirm.
- **client-flow**: 3 шага (Профиль → Клиент → Manifest) — горизонтальный степпер (иконки done/loading/locked, тонкая линия прогресса), кнопки scan/prepare-client c состояниями и поллингом, как в оригинале.
- SSE `useProfileEvents` → refetch списка.

- [ ] Build PASS; dev: создать тест-профиль, прогнать шаги, удалить → Commit: `feat(dashboard): профили`

### Task 13: Новости + чистка

**Files:** Rewrite `dashboard/app/news/page.tsx` (EmptyState «Раздел в разработке» в новом стиле); Delete: `dashboard/app/ui/` целиком (app-frame, sidebar, auth.ts, api-health, profile-admin, users-admin, anticheat-admin); Modify: все импорты `app/ui/auth` → `app/lib/api`.

- [ ] **Step 1:** Переписать news, удалить каталог `app/ui/`, починить импорты.
- [ ] **Step 2:** Проверки чистоты:

```bash
grep -rn "window.confirm\|material-symbols\|app/ui/" app/ components/ ; # пусто
grep -rn "style={{" app/ components/ | grep -v components/ui/ ;        # пусто (исключение — ui-примитивы)
npm run build   # PASS
```

- [ ] **Step 3:** Commit: `feat(dashboard): новости + удаление старого UI`

### Task 14: Живая приёмка

- [ ] Поднять backend локально (`docker compose up -d` + сидовые данные при необходимости) и `npm run dev`.
- [ ] Через chrome-devtools пройти чек-лист спеки §7: логин (ошибка→тост, успех), обзор (статкарты, статус API), профили (создание/редактирование/флоу), пользователи (бан с confirm+toast), античит (3 таба), Ctrl+K (раздел + игрок), hover-сайдбар, мобильная ширина 400px (бургер).
- [ ] Скриншоты ключевых экранов для пользователя.
- [ ] Commit оставшихся правок: `fix(dashboard): полировка по итогам приёмки`

## Self-review

- Покрытие спеки: токены (T1), api/типы/SSE (T2), примитивы (T3), тосты (T4), confirm (T5), shell+hover (T6), Ctrl+K (T7), все 6 страниц (T8–T13), приёмка (T14). Критерии чистоты — T13. ✓
- Типы консистентны: api<T>/ApiError (T2) используются всеми страницами; useToast/useConfirm сигнатуры заданы в T4/T5. ✓
- Порт-задачи ссылаются на чтение старых файлов в момент выполнения — исходники в репо, это источник логики, а не placeholder. ✓
