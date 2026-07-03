# Улучшенный UI просмотра скриншотов — план реализации

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Переработать вкладку «Скриншоты» админ-дашборда: галерея миниатюр, полноэкранный просмотрщик с зумом/листанием, фильтры, массовый запрос и запрос скриншота из вкладки «Детекты».

**Architecture:** Только фронтенд (`dashboard/`), бэкенд не меняется. Монолитный `screenshots-tab.tsx` разбивается на четыре файла: хук ленивой авторизованной загрузки изображений с общим кэшем objectURL, компонент галереи/таблицы, лайтбокс-просмотрщик и оркестрирующая вкладка. Онлайн-список сессий поднимается на уровень страницы, чтобы им пользовались и «Скриншоты», и «Детекты».

**Tech Stack:** Next.js 15 (App Router), React 19, Tailwind 4, lucide-react. Спека: `docs/superpowers/specs/2026-07-03-screenshots-ui-design.md`.

## Global Constraints

- Бэкенд и типы в `app/lib/types.ts` НЕ меняются. Эндпоинты: `GET /api/admin/anticheat/screenshots?limit=500`, `GET .../screenshots/:id/image` (JPEG, нужен Bearer), `POST .../screenshots` (`{nonce}` → 201 + запись `Screenshot`), `GET .../sessions/online`, `POST .../bans/account`.
- Никаких новых npm-зависимостей.
- Все тексты UI — на русском, стиль как в существующих вкладках.
- В дашборде НЕТ тест-фреймворка (eslint тоже не настроен). Проверка каждой задачи: `cd dashboard && npm run build` — тайпчек должен проходить без ошибок. TDD в классическом виде неприменим; шаг «build проходит» — обязательный гейт каждой задачи.
- Коммит после каждой задачи. В конце сообщения коммита: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- Существующие UI-примитивы: `Button` (`variant: primary|ghost|danger`, `loading`), `IconButton`, `Badge` (`tone: default|ok|warn|danger`), `Input`, `Table/Th/Td`, `Spinner size={n}`, `EmptyState icon={} title hint`, `useToast()(type, msg)`, `useConfirm()({title,message,confirmLabel,danger}) → Promise<boolean>`.

---

### Task 1: Хук ленивой загрузки изображений `use-screenshot-image.ts`

**Files:**
- Create: `dashboard/components/anticheat/use-screenshot-image.ts`

**Interfaces:**
- Consumes: `apiUrl`, `getToken` из `app/lib/api.ts`.
- Produces (используются задачами 2–4):
  - `useScreenshotImage(id: string | null, enabled?: boolean): { url: string | null; error: boolean }`
  - `clearScreenshotImageCache(): void`
  - `useInView<T extends Element>(): { ref: RefObject<T | null>; inView: boolean }`

- [ ] **Step 1: Создать файл с полным содержимым**

```ts
'use client';

import { useEffect, useRef, useState } from 'react';
import { apiUrl, getToken } from '../../app/lib/api';

// Кэш objectURL изображений скриншотов. Живёт на модуль: не сбрасывается при
// смене фильтров/вида, очищается clearScreenshotImageCache() при уходе со вкладки.
const cache = new Map<string, Promise<string>>();

async function fetchImage(id: string): Promise<string> {
  const resp = await fetch(`${apiUrl}/api/admin/anticheat/screenshots/${id}/image`, {
    headers: { Authorization: `Bearer ${getToken()}` }
  });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  const blob = await resp.blob();
  return URL.createObjectURL(blob);
}

/**
 * Загружает JPEG скриншота с Bearer-токеном (обычный img src не умеет
 * Authorization-заголовок) и отдаёт objectURL. enabled=false — не грузить
 * (ленивая загрузка: включается, когда карточка попала во вьюпорт).
 */
export function useScreenshotImage(id: string | null, enabled = true) {
  const [url, setUrl] = useState<string | null>(null);
  const [error, setError] = useState(false);

  useEffect(() => {
    if (!id || !enabled) return;
    let alive = true;
    let promise = cache.get(id);
    if (!promise) {
      promise = fetchImage(id);
      cache.set(id, promise);
      // Ошибку не кэшируем — следующий показ карточки повторит попытку.
      promise.catch(() => cache.delete(id));
    }
    promise.then(
      (u) => {
        if (alive) setUrl(u);
      },
      () => {
        if (alive) setError(true);
      }
    );
    return () => {
      alive = false;
    };
  }, [id, enabled]);

  return { url, error };
}

/** Revoke всех objectURL и очистка кэша — вызывается при размонтировании вкладки. */
export function clearScreenshotImageCache() {
  for (const promise of cache.values()) {
    void promise.then((u) => URL.revokeObjectURL(u)).catch(() => undefined);
  }
  cache.clear();
}

/** Однократный флаг «элемент попал во вьюпорт» (ленивая загрузка миниатюр). */
export function useInView<T extends Element>() {
  const ref = useRef<T | null>(null);
  const [inView, setInView] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el || inView) return;
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setInView(true);
      },
      { rootMargin: '200px' }
    );
    obs.observe(el);
    return () => obs.disconnect();
  }, [inView]);

  return { ref, inView };
}
```

- [ ] **Step 2: Проверить тайпчек**

Run: `cd dashboard && npm run build`
Expected: сборка успешна, ошибок TS нет (файл ещё никем не используется — просто компилируется).

- [ ] **Step 3: Commit**

```bash
git add dashboard/components/anticheat/use-screenshot-image.ts
git commit -m "feat(dashboard): хук ленивой авторизованной загрузки скриншотов с кэшем"
```

---

### Task 2: Галерея и таблица `screenshot-gallery.tsx`

**Files:**
- Create: `dashboard/components/anticheat/screenshot-gallery.tsx`

**Interfaces:**
- Consumes: `useScreenshotImage`, `useInView` из Task 1; тип `Screenshot` из `app/lib/types.ts`.
- Produces (используются задачей 4):
  - `ScreenshotGallery({ screenshots, onOpen }: { screenshots: Screenshot[]; onOpen: (s: Screenshot) => void })`
  - `ScreenshotTable({ screenshots, onOpen }: { screenshots: Screenshot[]; onOpen: (s: Screenshot) => void })`
  - `SCREENSHOT_STATUS_LABELS: Record<string, string>`, `screenshotStatusTone(status: string)`

- [ ] **Step 1: Создать файл с полным содержимым**

```tsx
'use client';

import { AlertTriangle, ImageOff } from 'lucide-react';
import type { Screenshot } from '../../app/lib/types';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { Spinner } from '../ui/spinner';
import { Table, Th, Td } from '../ui/table';
import { useInView, useScreenshotImage } from './use-screenshot-image';

export const SCREENSHOT_STATUS_LABELS: Record<string, string> = {
  pending: 'Ожидание',
  capturing: 'Захват',
  done: 'Готов',
  failed: 'Ошибка'
};

export function screenshotStatusTone(status: string): 'ok' | 'warn' | 'danger' | 'default' {
  if (status === 'done') return 'ok';
  if (status === 'failed') return 'danger';
  if (status === 'pending' || status === 'capturing') return 'warn';
  return 'default';
}

/**
 * Ленивая миниатюра: JPEG грузится только когда карточка попала во вьюпорт.
 * Для незавершённых показывает спиннер со статусом, для failed — причину.
 */
function Thumb({ shot }: { shot: Screenshot }) {
  const { ref, inView } = useInView<HTMLDivElement>();
  const { url, error } = useScreenshotImage(shot.status === 'done' ? shot.id : null, inView);

  return (
    <div ref={ref} className="flex h-full w-full items-center justify-center overflow-hidden bg-surface">
      {shot.status === 'done' ? (
        url ? (
          // eslint-disable-next-line @next/next/no-img-element
          <img src={url} alt={`Скриншот ${shot.login || shot.userUuid}`} className="h-full w-full object-cover" />
        ) : error ? (
          <ImageOff size={20} className="text-fg-faint" />
        ) : (
          <Spinner size={18} />
        )
      ) : shot.status === 'failed' ? (
        <div className="flex flex-col items-center gap-1 px-2 text-center">
          <AlertTriangle size={18} className="text-danger" />
          <span className="text-xs text-fg-muted">{shot.error || 'не удался'}</span>
        </div>
      ) : (
        <div className="flex flex-col items-center gap-1.5">
          <Spinner size={18} />
          <span className="text-xs text-fg-muted">{SCREENSHOT_STATUS_LABELS[shot.status] ?? shot.status}</span>
        </div>
      )}
    </div>
  );
}

/** Сетка карточек-превью. Клик по готовому скриншоту открывает просмотрщик. */
export function ScreenshotGallery({
  screenshots,
  onOpen
}: {
  screenshots: Screenshot[];
  onOpen: (s: Screenshot) => void;
}) {
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5">
      {screenshots.map((s) => (
        <button
          key={s.id}
          onClick={() => onOpen(s)}
          disabled={s.status !== 'done'}
          className="group overflow-hidden rounded-xl border border-edge bg-bg/50 text-left transition enabled:hover:border-edge-strong disabled:cursor-default"
        >
          <div className="aspect-video w-full">
            <Thumb shot={s} />
          </div>
          <div className="flex flex-col gap-1 border-t border-edge p-2.5">
            <div className="flex items-center justify-between gap-2">
              <span className="truncate text-sm font-medium">{s.login || s.userUuid}</span>
              <Badge tone={screenshotStatusTone(s.status)}>
                {SCREENSHOT_STATUS_LABELS[s.status] ?? s.status}
              </Badge>
            </div>
            <div className="truncate text-xs text-fg-muted">
              {new Date(s.createdAt).toLocaleString('ru-RU')}
              {s.requestedBy ? ` · ${s.requestedBy}` : ''}
            </div>
          </div>
        </button>
      ))}
    </div>
  );
}

/** Табличный вид: прежняя таблица плюс колонка мини-превью. */
export function ScreenshotTable({
  screenshots,
  onOpen
}: {
  screenshots: Screenshot[];
  onOpen: (s: Screenshot) => void;
}) {
  return (
    <Table>
      <thead>
        <tr>
          <Th>Превью</Th>
          <Th>Игрок</Th>
          <Th>Статус</Th>
          <Th>Размер</Th>
          <Th>Кто запросил</Th>
          <Th>Дата</Th>
          <Th />
        </tr>
      </thead>
      <tbody>
        {screenshots.map((s) => (
          <tr key={s.id}>
            <Td>
              <button
                onClick={() => onOpen(s)}
                disabled={s.status !== 'done'}
                className="block h-12 w-20 overflow-hidden rounded-md border border-edge disabled:cursor-default"
              >
                <Thumb shot={s} />
              </button>
            </Td>
            <Td className="font-medium">{s.login || s.userUuid}</Td>
            <Td>
              <Badge tone={screenshotStatusTone(s.status)}>
                {SCREENSHOT_STATUS_LABELS[s.status] ?? s.status}
              </Badge>
            </Td>
            <Td className="text-fg-secondary">
              {s.status === 'done' && s.width > 0
                ? `${s.width}×${s.height} · ${(s.size / 1024).toFixed(0)} КБ`
                : s.error || '—'}
            </Td>
            <Td className="text-fg-secondary">{s.requestedBy || '—'}</Td>
            <Td className="whitespace-nowrap text-fg-muted">
              {new Date(s.createdAt).toLocaleString('ru-RU')}
            </Td>
            <Td className="text-right">
              {s.status === 'done' && (
                <Button variant="ghost" className="h-8 px-3" onClick={() => onOpen(s)}>
                  Посмотреть
                </Button>
              )}
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}
```

- [ ] **Step 2: Проверить тайпчек**

Run: `cd dashboard && npm run build`
Expected: сборка успешна.

- [ ] **Step 3: Commit**

```bash
git add dashboard/components/anticheat/screenshot-gallery.tsx
git commit -m "feat(dashboard): галерея и таблица скриншотов с ленивыми миниатюрами"
```

---

### Task 3: Лайтбокс `screenshot-viewer.tsx`

**Files:**
- Create: `dashboard/components/anticheat/screenshot-viewer.tsx`

**Interfaces:**
- Consumes: `useScreenshotImage` из Task 1; тип `Screenshot`.
- Produces (используется задачей 4):

```ts
ScreenshotViewer(props: {
  items: Screenshot[];          // только status==='done', в порядке фильтра
  index: number;                // текущий кадр
  onNavigate: (index: number) => void;
  onClose: () => void;
  canRequestMore: boolean;      // игрок текущего кадра сейчас онлайн
  onRequestMore: () => void;
  onGoToDetections: () => void;
  onBan: () => void;
})
```

- [ ] **Step 1: Создать файл с полным содержимым**

```tsx
'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Camera,
  ChevronLeft,
  ChevronRight,
  Download,
  Maximize,
  Minus,
  Plus,
  ShieldAlert,
  Ban,
  X
} from 'lucide-react';
import type { Screenshot } from '../../app/lib/types';
import { Spinner } from '../ui/spinner';
import { useScreenshotImage } from './use-screenshot-image';

type Transform = { scale: number; x: number; y: number };
const FIT: Transform = { scale: 1, x: 0, y: 0 };
const MAX_SCALE = 8;

/** Кнопка тулбара на тёмном оверлее (стили Button заточены под светлую тему). */
function TBtn({
  title,
  onClick,
  danger = false,
  children
}: {
  title: string;
  onClick: () => void;
  danger?: boolean;
  children: React.ReactNode;
}) {
  return (
    <button
      title={title}
      aria-label={title}
      onClick={onClick}
      className={`inline-flex h-9 w-9 items-center justify-center rounded-lg border border-white/15 bg-white/5 transition hover:bg-white/15 ${
        danger ? 'text-red-400' : 'text-white'
      }`}
    >
      {children}
    </button>
  );
}

function downloadName(s: Screenshot): string {
  const d = new Date(s.createdAt);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${s.login || s.userUuid}_${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}_${pad(
    d.getHours()
  )}-${pad(d.getMinutes())}.jpg`;
}

/**
 * Полноэкранный просмотрщик: зум колесом к курсору, drag-пан, двойной клик
 * fit⇄100%, листание ←/→ по отфильтрованным done-скриншотам, скачивание,
 * действия по игроку. CSS transform, без зависимостей.
 */
export function ScreenshotViewer({
  items,
  index,
  onNavigate,
  onClose,
  canRequestMore,
  onRequestMore,
  onGoToDetections,
  onBan
}: {
  items: Screenshot[];
  index: number;
  onNavigate: (index: number) => void;
  onClose: () => void;
  canRequestMore: boolean;
  onRequestMore: () => void;
  onGoToDetections: () => void;
  onBan: () => void;
}) {
  const shot = items[index] ?? null;
  const { url, error } = useScreenshotImage(shot?.id ?? null);
  // Прелоад соседей — мгновенное листание.
  useScreenshotImage(items[index - 1]?.id ?? null);
  useScreenshotImage(items[index + 1]?.id ?? null);

  const [t, setT] = useState<Transform>(FIT);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const imgRef = useRef<HTMLImageElement | null>(null);
  const drag = useRef<{ pointerId: number; startX: number; startY: number; baseX: number; baseY: number } | null>(
    null
  );

  // Сброс зума при смене кадра.
  useEffect(() => setT(FIT), [index]);

  // Блокируем прокрутку страницы под оверлеем (заодно снимает проблему
  // passive wheel-листенеров React — preventDefault не нужен).
  useEffect(() => {
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = prev;
    };
  }, []);

  const goPrev = useCallback(() => {
    if (index > 0) onNavigate(index - 1);
  }, [index, onNavigate]);
  const goNext = useCallback(() => {
    if (index < items.length - 1) onNavigate(index + 1);
  }, [index, items.length, onNavigate]);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose();
      else if (e.key === 'ArrowLeft') goPrev();
      else if (e.key === 'ArrowRight') goNext();
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose, goPrev, goNext]);

  // Зум так, чтобы точка под курсором осталась на месте.
  // Экранная точка кадра: p = t + scale * p_img  ⇒  t' = c − k·(c − t), k = scale'/scale.
  const zoomAt = useCallback((clientX: number, clientY: number, factor: number) => {
    const rect = containerRef.current?.getBoundingClientRect();
    if (!rect) return;
    const cx = clientX - rect.left - rect.width / 2;
    const cy = clientY - rect.top - rect.height / 2;
    setT((prev) => {
      const scale = Math.min(MAX_SCALE, Math.max(1, prev.scale * factor));
      if (scale === prev.scale) return prev;
      if (scale === 1) return FIT;
      const k = scale / prev.scale;
      return { scale, x: cx - k * (cx - prev.x), y: cy - k * (cy - prev.y) };
    });
  }, []);

  function zoomCenter(factor: number) {
    const rect = containerRef.current?.getBoundingClientRect();
    if (!rect) return;
    zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, factor);
  }

  function onWheel(e: React.WheelEvent) {
    zoomAt(e.clientX, e.clientY, e.deltaY < 0 ? 1.2 : 1 / 1.2);
  }

  function onDoubleClick(e: React.MouseEvent) {
    if (t.scale > 1) {
      setT(FIT);
      return;
    }
    // fit → 100%: во сколько раз натуральный размер больше вписанного.
    const img = imgRef.current;
    if (!img || !img.naturalWidth) return;
    const renderedWidth = img.getBoundingClientRect().width;
    if (renderedWidth <= 0) return;
    zoomAt(e.clientX, e.clientY, Math.max(1.01, img.naturalWidth / renderedWidth));
  }

  function onPointerDown(e: React.PointerEvent) {
    if (t.scale <= 1) return;
    drag.current = { pointerId: e.pointerId, startX: e.clientX, startY: e.clientY, baseX: t.x, baseY: t.y };
    (e.target as Element).setPointerCapture(e.pointerId);
  }

  function onPointerMove(e: React.PointerEvent) {
    const d = drag.current;
    if (!d || d.pointerId !== e.pointerId) return;
    setT((prev) => ({ ...prev, x: d.baseX + (e.clientX - d.startX), y: d.baseY + (e.clientY - d.startY) }));
  }

  function onPointerUp(e: React.PointerEvent) {
    if (drag.current?.pointerId === e.pointerId) drag.current = null;
  }

  function download() {
    if (!url || !shot) return;
    const a = document.createElement('a');
    a.href = url;
    a.download = downloadName(shot);
    a.click();
  }

  if (!shot) return null;

  return (
    <div className="fixed inset-0 z-50 flex flex-col bg-black/90 backdrop-blur-sm">
      {/* Тулбар */}
      <div className="flex items-center gap-2 border-b border-white/10 px-4 py-2.5">
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold text-white">{shot.login || shot.userUuid}</div>
          <div className="truncate text-xs text-white/60">
            {new Date(shot.createdAt).toLocaleString('ru-RU')} · {shot.width}×{shot.height} ·{' '}
            {(shot.size / 1024).toFixed(0)} КБ
            {shot.requestedBy ? ` · запросил ${shot.requestedBy}` : ''}
          </div>
        </div>
        <span className="whitespace-nowrap text-xs text-white/60">
          {index + 1} / {items.length}
        </span>
        <TBtn title="Отдалить" onClick={() => zoomCenter(1 / 1.4)}>
          <Minus size={16} />
        </TBtn>
        <TBtn title="Приблизить" onClick={() => zoomCenter(1.4)}>
          <Plus size={16} />
        </TBtn>
        <TBtn title="Вписать в экран" onClick={() => setT(FIT)}>
          <Maximize size={16} />
        </TBtn>
        <TBtn title="Скачать JPEG" onClick={download}>
          <Download size={16} />
        </TBtn>
        {canRequestMore && (
          <TBtn title="Запросить ещё скриншот" onClick={onRequestMore}>
            <Camera size={16} />
          </TBtn>
        )}
        <TBtn title="Детекты игрока" onClick={onGoToDetections}>
          <ShieldAlert size={16} />
        </TBtn>
        <TBtn title="Забанить аккаунт" onClick={onBan} danger>
          <Ban size={16} />
        </TBtn>
        <TBtn title="Закрыть (Esc)" onClick={onClose}>
          <X size={16} />
        </TBtn>
      </div>

      {/* Сцена */}
      <div
        ref={containerRef}
        className="relative flex-1 touch-none overflow-hidden"
        onWheel={onWheel}
        onDoubleClick={onDoubleClick}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
      >
        <div className="flex h-full w-full items-center justify-center">
          {url ? (
            // eslint-disable-next-line @next/next/no-img-element
            <img
              ref={imgRef}
              src={url}
              alt={`Скриншот ${shot.login || shot.userUuid}`}
              draggable={false}
              style={{ transform: `translate(${t.x}px, ${t.y}px) scale(${t.scale})` }}
              className={`max-h-full max-w-full select-none ${t.scale > 1 ? 'cursor-grab' : 'cursor-zoom-in'}`}
            />
          ) : error ? (
            <span className="text-sm text-white/70">Не удалось загрузить изображение</span>
          ) : (
            <Spinner size={24} />
          )}
        </div>

        {index > 0 && (
          <button
            aria-label="Предыдущий"
            onClick={goPrev}
            className="absolute left-3 top-1/2 -translate-y-1/2 rounded-full border border-white/15 bg-black/50 p-2 text-white transition hover:bg-black/80"
          >
            <ChevronLeft size={22} />
          </button>
        )}
        {index < items.length - 1 && (
          <button
            aria-label="Следующий"
            onClick={goNext}
            className="absolute right-3 top-1/2 -translate-y-1/2 rounded-full border border-white/15 bg-black/50 p-2 text-white transition hover:bg-black/80"
          >
            <ChevronRight size={22} />
          </button>
        )}
      </div>
    </div>
  );
}
```

- [ ] **Step 2: Проверить тайпчек**

Run: `cd dashboard && npm run build`
Expected: сборка успешна.

- [ ] **Step 3: Commit**

```bash
git add dashboard/components/anticheat/screenshot-viewer.tsx
git commit -m "feat(dashboard): полноэкранный просмотрщик скриншотов (зум, пан, листание)"
```

---

### Task 4: Переработка `screenshots-tab.tsx` + подъём онлайн-списка в `page.tsx`

**Files:**
- Modify: `dashboard/components/anticheat/screenshots-tab.tsx` (полная замена содержимого)
- Modify: `dashboard/app/anticheat/page.tsx`

**Interfaces:**
- Consumes: `ScreenshotGallery`, `ScreenshotTable` (Task 2), `ScreenshotViewer` (Task 3), `clearScreenshotImageCache` (Task 1).
- Produces: новый пропс-контракт `ScreenshotsTab` (см. код) — `page.tsx` обновляется в этой же задаче. Пропс `onOpenDetections(login)` пока только переключает вкладку; префильтр детектов добавит Task 5.

- [ ] **Step 1: Полностью заменить `screenshots-tab.tsx`**

```tsx
'use client';

import { useEffect, useMemo, useState } from 'react';
import { Camera, ImageIcon, LayoutGrid, List, Monitor, RefreshCw } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { OnlineSession, Screenshot } from '../../app/lib/types';
import { Button } from '../ui/button';
import { Input } from '../ui/input';
import { SkeletonTable } from '../ui/skeleton';
import { EmptyState } from '../ui/empty-state';
import { useToast } from '../ui/toast';
import { useConfirm } from '../ui/confirm';
import { ScreenshotGallery, ScreenshotTable } from './screenshot-gallery';
import { ScreenshotViewer } from './screenshot-viewer';
import { clearScreenshotImageCache } from './use-screenshot-image';

type StatusFilter = 'all' | 'done' | 'active' | 'failed';
type DateFilter = 'all' | 'today' | '7d' | '30d';
type ViewMode = 'grid' | 'table';

const VIEW_KEY = 'anticheat.screenshots.view';

const STATUS_FILTERS: { key: StatusFilter; label: string }[] = [
  { key: 'all', label: 'Все' },
  { key: 'done', label: 'Готовые' },
  { key: 'active', label: 'В процессе' },
  { key: 'failed', label: 'Ошибки' }
];

const DATE_FILTERS: { key: DateFilter; label: string }[] = [
  { key: 'all', label: 'Всё время' },
  { key: 'today', label: 'Сегодня' },
  { key: '7d', label: '7 дней' },
  { key: '30d', label: '30 дней' }
];

function dateCutoff(filter: DateFilter): number | null {
  if (filter === 'all') return null;
  if (filter === 'today') {
    const d = new Date();
    d.setHours(0, 0, 0, 0);
    return d.getTime();
  }
  const days = filter === '7d' ? 7 : 30;
  return Date.now() - days * 24 * 60 * 60 * 1000;
}

function Chip({
  active,
  onClick,
  children
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      className={`h-8 rounded-lg border px-3 text-xs font-medium transition ${
        active ? 'border-fg bg-fg text-bg' : 'border-edge text-fg-secondary hover:text-fg'
      }`}
    >
      {children}
    </button>
  );
}

export function ScreenshotsTab({
  screenshots,
  loading,
  onReload,
  online,
  onlineLoading,
  onReloadOnline,
  onOpenDetections
}: {
  screenshots: Screenshot[];
  loading: boolean;
  onReload: () => Promise<void>;
  online: OnlineSession[];
  onlineLoading: boolean;
  onReloadOnline: () => Promise<void>;
  onOpenDetections: (login: string) => void;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const [requesting, setRequesting] = useState<string | null>(null); // nonce
  const [bulkRunning, setBulkRunning] = useState(false);
  const [awaitedId, setAwaitedId] = useState<string | null>(null);
  const [viewerId, setViewerId] = useState<string | null>(null);

  const [loginQuery, setLoginQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all');
  const [dateFilter, setDateFilter] = useState<DateFilter>('all');
  const [view, setView] = useState<ViewMode>('grid');

  // Восстановление вида после маунта (SSR-safe: на сервере localStorage нет).
  useEffect(() => {
    const saved = window.localStorage.getItem(VIEW_KEY);
    if (saved === 'grid' || saved === 'table') setView(saved);
  }, []);

  function changeView(v: ViewMode) {
    setView(v);
    window.localStorage.setItem(VIEW_KEY, v);
  }

  // Кэш objectURL живёт, пока открыта вкладка; revoke при уходе.
  useEffect(() => clearScreenshotImageCache, []);

  // Поллинг: пока есть pending/capturing скриншоты, обновляем каждые 2с.
  useEffect(() => {
    const hasPending = screenshots.some((s) => s.status === 'pending' || s.status === 'capturing');
    if (!hasPending) return;
    const id = setInterval(() => void onReload(), 2000);
    return () => clearInterval(id);
  }, [screenshots, onReload]);

  // Обновляем онлайн-список при изменении числа скриншотов (игрок мог выйти).
  useEffect(() => {
    void onReloadOnline();
  }, [onReloadOnline, screenshots.length]);

  const filtered = useMemo(() => {
    const q = loginQuery.trim().toLowerCase();
    const cutoff = dateCutoff(dateFilter);
    return screenshots.filter((s) => {
      if (q && !(s.login || s.userUuid).toLowerCase().includes(q)) return false;
      if (statusFilter === 'done' && s.status !== 'done') return false;
      if (statusFilter === 'active' && s.status !== 'pending' && s.status !== 'capturing') return false;
      if (statusFilter === 'failed' && s.status !== 'failed') return false;
      if (cutoff !== null && new Date(s.createdAt).getTime() < cutoff) return false;
      return true;
    });
  }, [screenshots, loginQuery, statusFilter, dateFilter]);

  // Просмотрщик листает только готовые из текущего фильтра.
  const doneItems = useMemo(() => filtered.filter((s) => s.status === 'done'), [filtered]);
  const viewerIndex = viewerId === null ? -1 : doneItems.findIndex((s) => s.id === viewerId);
  const viewerShot = viewerIndex >= 0 ? doneItems[viewerIndex] : null;
  const viewerSession = viewerShot ? online.find((o) => o.uuid === viewerShot.userUuid) : undefined;

  // Автооткрытие: одиночный запрос дозрел до done → открываем просмотрщик.
  useEffect(() => {
    if (!awaitedId) return;
    const rec = screenshots.find((s) => s.id === awaitedId);
    if (!rec) return;
    if (rec.status === 'done') {
      setViewerId(rec.id);
      setAwaitedId(null);
    } else if (rec.status === 'failed') {
      toast('error', `Скриншот ${rec.login || rec.userUuid}: ${rec.error || 'не удался'}`);
      setAwaitedId(null);
    }
  }, [screenshots, awaitedId, toast]);

  async function requestShot(sess: OnlineSession, awaitResult: boolean) {
    setRequesting(sess.nonce);
    try {
      const rec = await api<Screenshot>('/api/admin/anticheat/screenshots', {
        method: 'POST',
        body: { nonce: sess.nonce }
      });
      toast('success', `Запрос скриншота отправлен: ${sess.login}`);
      if (awaitResult && rec?.id) setAwaitedId(rec.id);
      await Promise.all([onReload(), onReloadOnline()]);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setRequesting(null);
    }
  }

  async function requestAll() {
    setBulkRunning(true);
    const total = online.length;
    let ok = 0;
    for (const sess of online) {
      try {
        await api('/api/admin/anticheat/screenshots', { method: 'POST', body: { nonce: sess.nonce } });
        ok++;
      } catch {
        // продолжаем серию, итог — в суммарном тосте
      }
    }
    toast(ok === total ? 'success' : 'error', `Запрошено ${ok} из ${total}`);
    await Promise.all([onReload(), onReloadOnline()]);
    setBulkRunning(false);
  }

  async function banFromViewer() {
    if (!viewerShot) return;
    const target = viewerShot.login || viewerShot.userUuid;
    const ok = await confirm({
      title: 'Забанить аккаунт',
      message: `Забанить аккаунт ${target}?`,
      confirmLabel: 'Забанить',
      danger: true
    });
    if (!ok) return;
    try {
      await api('/api/admin/anticheat/bans/account', {
        method: 'POST',
        body: { userUuid: viewerShot.userUuid, login: viewerShot.login, reason: 'manual: по скриншоту' }
      });
      toast('success', `Аккаунт ${target} забанен`);
    } catch (e) {
      toast('error', errorMessage(e));
    }
  }

  return (
    <div className="flex flex-col gap-4">
      {/* Онлайн-игроки */}
      <div className="rounded-xl border border-edge bg-bg/50 p-4">
        <div className="mb-3 flex items-center justify-between gap-2">
          <h3 className="text-sm font-semibold">Онлайн-игроки ({online.length})</h3>
          <div className="flex items-center gap-2">
            {online.length > 0 && (
              <Button variant="primary" className="h-8 px-3" loading={bulkRunning} onClick={() => void requestAll()}>
                <Camera size={14} />
                <span className="ml-1.5">Скриншот у всех</span>
              </Button>
            )}
            <Button variant="ghost" className="h-8 px-3" loading={onlineLoading} onClick={() => void onReloadOnline()}>
              <RefreshCw size={14} />
              <span className="ml-1.5">Обновить</span>
            </Button>
          </div>
        </div>
        {online.length === 0 ? (
          <p className="py-4 text-center text-sm text-fg-muted">Нет игроков онлайн</p>
        ) : (
          <div className="flex flex-wrap gap-2">
            {online.map((s) => (
              <div key={s.nonce} className="flex items-center gap-2 rounded-lg border border-edge bg-bg px-3 py-2">
                <Monitor size={16} className="text-fg-muted" />
                <span className="text-sm font-medium">{s.login}</span>
                {s.ipAddress && <span className="font-mono text-xs text-fg-muted">{s.ipAddress}</span>}
                <Button
                  variant="primary"
                  className="h-7 px-2.5"
                  loading={requesting === s.nonce}
                  onClick={() => void requestShot(s, true)}
                >
                  <Camera size={14} />
                  <span className="ml-1">Скриншот</span>
                </Button>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Фильтры и переключатель вида */}
      <div className="flex flex-wrap items-center gap-2">
        <Input
          value={loginQuery}
          onChange={(e) => setLoginQuery(e.target.value)}
          placeholder="Поиск по логину…"
          className="h-8 w-44 text-xs"
          aria-label="Поиск по логину"
        />
        <div className="flex items-center gap-1">
          {STATUS_FILTERS.map((f) => (
            <Chip key={f.key} active={statusFilter === f.key} onClick={() => setStatusFilter(f.key)}>
              {f.label}
            </Chip>
          ))}
        </div>
        <div className="flex items-center gap-1">
          {DATE_FILTERS.map((f) => (
            <Chip key={f.key} active={dateFilter === f.key} onClick={() => setDateFilter(f.key)}>
              {f.label}
            </Chip>
          ))}
        </div>
        <span className="text-xs text-fg-muted">
          показано {filtered.length} из {screenshots.length}
        </span>
        <div className="ml-auto inline-flex items-center gap-0.5 rounded-lg border border-edge p-0.5">
          <button
            aria-label="Галерея"
            title="Галерея"
            onClick={() => changeView('grid')}
            className={`inline-flex h-7 w-8 items-center justify-center rounded-md transition ${
              view === 'grid' ? 'bg-fg text-bg' : 'text-fg-secondary hover:text-fg'
            }`}
          >
            <LayoutGrid size={14} />
          </button>
          <button
            aria-label="Таблица"
            title="Таблица"
            onClick={() => changeView('table')}
            className={`inline-flex h-7 w-8 items-center justify-center rounded-md transition ${
              view === 'table' ? 'bg-fg text-bg' : 'text-fg-secondary hover:text-fg'
            }`}
          >
            <List size={14} />
          </button>
        </div>
      </div>

      {/* Список */}
      {loading ? (
        <SkeletonTable rows={5} cols={6} />
      ) : screenshots.length === 0 ? (
        <EmptyState icon={ImageIcon} title="Скриншотов нет" hint="Выберите онлайн-игрока и запросите скриншот экрана." />
      ) : filtered.length === 0 ? (
        <EmptyState icon={ImageIcon} title="Ничего не найдено" hint="Под выбранные фильтры ничего не подходит." />
      ) : view === 'grid' ? (
        <ScreenshotGallery screenshots={filtered} onOpen={(s) => setViewerId(s.id)} />
      ) : (
        <ScreenshotTable screenshots={filtered} onOpen={(s) => setViewerId(s.id)} />
      )}

      {viewerShot && (
        <ScreenshotViewer
          items={doneItems}
          index={viewerIndex}
          onNavigate={(i) => setViewerId(doneItems[i].id)}
          onClose={() => setViewerId(null)}
          canRequestMore={!!viewerSession}
          onRequestMore={() => {
            if (!viewerSession) return;
            setViewerId(null);
            void requestShot(viewerSession, true);
          }}
          onGoToDetections={() => {
            const login = viewerShot.login || viewerShot.userUuid;
            setViewerId(null);
            onOpenDetections(login);
          }}
          onBan={() => void banFromViewer()}
        />
      )}
    </div>
  );
}
```

- [ ] **Step 2: Обновить `page.tsx`**

Полная замена содержимого `dashboard/app/anticheat/page.tsx`:

```tsx
'use client';

import { useCallback, useEffect, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import type {
  AccountBan,
  CheatSignature,
  Detection,
  HwidBan,
  OnlineSession,
  Screenshot,
  SignatureStat
} from '../lib/types';
import { Tabs } from '../../components/ui/tabs';
import { useToast } from '../../components/ui/toast';
import { DetectionsTab } from '../../components/anticheat/detections-tab';
import { BansTab } from '../../components/anticheat/bans-tab';
import { SignaturesTab } from '../../components/anticheat/signatures-tab';
import { StatsTab } from '../../components/anticheat/stats-tab';
import { ScreenshotsTab } from '../../components/anticheat/screenshots-tab';

export default function AnticheatPage() {
  const toast = useToast();
  const [tab, setTab] = useState('detections');
  const [loading, setLoading] = useState(true);

  const [detections, setDetections] = useState<Detection[]>([]);
  const [accountBans, setAccountBans] = useState<AccountBan[]>([]);
  const [hwidBans, setHwidBans] = useState<HwidBan[]>([]);
  const [signatures, setSignatures] = useState<CheatSignature[]>([]);
  const [stats, setStats] = useState<SignatureStat[]>([]);
  const [screenshots, setScreenshots] = useState<Screenshot[]>([]);

  // Онлайн-сессии нужны и «Скриншотам», и «Детектам» (кнопка камеры) —
  // поэтому живут на уровне страницы.
  const [online, setOnline] = useState<OnlineSession[]>([]);
  const [onlineLoading, setOnlineLoading] = useState(false);

  const reload = useCallback(async () => {
    try {
      const [det, ab, hb, sigs, st, shots] = await Promise.all([
        api<Detection[]>('/api/admin/anticheat/detections?limit=200'),
        api<AccountBan[]>('/api/admin/anticheat/bans/account'),
        api<HwidBan[]>('/api/admin/anticheat/bans/hwid'),
        api<CheatSignature[]>('/api/admin/anticheat/signatures'),
        api<SignatureStat[]>('/api/admin/anticheat/stats?days=7'),
        api<Screenshot[]>('/api/admin/anticheat/screenshots?limit=500')
      ]);
      setDetections(det ?? []);
      setAccountBans(ab ?? []);
      setHwidBans(hb ?? []);
      setSignatures(sigs ?? []);
      setStats(st ?? []);
      setScreenshots(shots ?? []);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [toast]);

  const reloadOnline = useCallback(async () => {
    setOnlineLoading(true);
    try {
      const list = await api<OnlineSession[]>('/api/admin/anticheat/sessions/online');
      setOnline(list ?? []);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setOnlineLoading(false);
    }
  }, [toast]);

  useEffect(() => {
    void reload();
    void reloadOnline();
  }, [reload, reloadOnline]);

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-bold">Античит</h1>
        <p className="mt-0.5 text-sm text-fg-muted">Детекты, баны и сигнатуры читов</p>
      </div>

      <Tabs
        items={[
          { key: 'detections', label: 'Детекты', badge: detections.length },
          { key: 'bans', label: 'Баны', badge: accountBans.length + hwidBans.length },
          { key: 'signatures', label: 'Сигнатуры', badge: signatures.length },
          { key: 'screenshots', label: 'Скриншоты', badge: screenshots.length },
          { key: 'stats', label: 'Статистика', badge: stats.length }
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === 'detections' && <DetectionsTab detections={detections} loading={loading} onReload={reload} />}
      {tab === 'bans' && (
        <BansTab accountBans={accountBans} hwidBans={hwidBans} loading={loading} onReload={reload} />
      )}
      {tab === 'signatures' && <SignaturesTab signatures={signatures} loading={loading} onReload={reload} />}
      {tab === 'screenshots' && (
        <ScreenshotsTab
          screenshots={screenshots}
          loading={loading}
          onReload={reload}
          online={online}
          onlineLoading={onlineLoading}
          onReloadOnline={reloadOnline}
          onOpenDetections={() => setTab('detections')}
        />
      )}
      {tab === 'stats' && <StatsTab stats={stats} loading={loading} />}
    </div>
  );
}
```

Примечание: `onOpenDetections` пока игнорирует логин и просто переключает вкладку — префильтр по логину добавляет Task 5.

- [ ] **Step 3: Проверить тайпчек**

Run: `cd dashboard && npm run build`
Expected: сборка успешна.

- [ ] **Step 4: Commit**

```bash
git add dashboard/components/anticheat/screenshots-tab.tsx dashboard/app/anticheat/page.tsx
git commit -m "feat(dashboard): вкладка скриншотов — фильтры, галерея/таблица, лайтбокс, массовый запрос"
```

---

### Task 5: Кнопка камеры и префильтр логина во вкладке «Детекты»

**Files:**
- Modify: `dashboard/components/anticheat/detections-tab.tsx`
- Modify: `dashboard/app/anticheat/page.tsx`

**Interfaces:**
- Consumes: `OnlineSession` из types, существующие фильтры detections-tab.
- Produces: новые пропсы `DetectionsTab`: `online: OnlineSession[]`, `initialLoginFilter?: string`.

- [ ] **Step 1: Обновить `detections-tab.tsx`**

Изменения (точечные, остальное без правок):

1. Импорты — добавить `useEffect`, иконку `Camera`, тип `OnlineSession`, компонент `Input`:

```tsx
import { useEffect, useMemo, useState } from 'react';
import { Camera, ShieldCheck } from 'lucide-react';
import type { Detection, DetectionStatus, OnlineSession } from '../../app/lib/types';
import { Input } from '../ui/input';
```

2. Сигнатура компонента:

```tsx
export function DetectionsTab({
  detections,
  loading,
  onReload,
  online,
  initialLoginFilter
}: {
  detections: Detection[];
  loading: boolean;
  onReload: () => Promise<void>;
  online: OnlineSession[];
  initialLoginFilter?: string;
}) {
```

3. Состояние — после `const [statusFilter, setStatusFilter] = useState('');` добавить:

```tsx
  const [loginFilter, setLoginFilter] = useState(initialLoginFilter ?? '');
  const [shotRequestingId, setShotRequestingId] = useState<string | null>(null);

  // Префильтр из просмотрщика скриншотов: «Детекты игрока» обновляет проп.
  useEffect(() => {
    if (initialLoginFilter !== undefined && initialLoginFilter !== '') setLoginFilter(initialLoginFilter);
  }, [initialLoginFilter]);
```

4. `filtered` — добавить условие по логину:

```tsx
  const filtered = useMemo(() => {
    const q = loginFilter.trim().toLowerCase();
    return detections.filter(
      (d) =>
        (confidenceFilter === '' || d.confidence === confidenceFilter) &&
        (statusFilter === '' || d.status === statusFilter) &&
        (q === '' || (d.login || d.userUuid).toLowerCase().includes(q))
    );
  }, [detections, confidenceFilter, statusFilter, loginFilter]);
```

5. Функция запроса скриншота — после `banHwid`:

```tsx
  async function requestScreenshot(d: Detection) {
    const sess = online.find((o) => o.uuid === d.userUuid);
    if (!sess) return;
    setShotRequestingId(d.id);
    try {
      await api('/api/admin/anticheat/screenshots', { method: 'POST', body: { nonce: sess.nonce } });
      toast('success', `Запрос скриншота отправлен: ${sess.login}`);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setShotRequestingId(null);
    }
  }
```

6. Блок фильтров — в начало `<div className="flex flex-wrap gap-2">` добавить поле поиска:

```tsx
        <Input
          value={loginFilter}
          onChange={(e) => setLoginFilter(e.target.value)}
          placeholder="Поиск по логину…"
          className="h-9 w-44 text-sm"
          aria-label="Поиск по логину"
        />
```

7. В ячейке действий (`<div className="flex justify-end gap-2">`) — перед кнопкой «Бан» добавить кнопку камеры (видна только если игрок онлайн):

```tsx
                    {online.some((o) => o.uuid === d.userUuid) && (
                      <Button
                        variant="ghost"
                        className="h-8 px-2.5"
                        title="Запросить скриншот экрана"
                        loading={shotRequestingId === d.id}
                        onClick={() => void requestScreenshot(d)}
                      >
                        <Camera size={14} />
                      </Button>
                    )}
```

- [ ] **Step 2: Обновить `page.tsx` — состояние префильтра и проброс пропсов**

В `AnticheatPage`:

1. Добавить состояние после `const [onlineLoading, setOnlineLoading] = useState(false);`:

```tsx
  // Префильтр детектов по логину (переход из просмотрщика скриншотов).
  const [detectionsLogin, setDetectionsLogin] = useState('');
```

2. Заменить рендер `DetectionsTab`:

```tsx
      {tab === 'detections' && (
        <DetectionsTab
          detections={detections}
          loading={loading}
          onReload={reload}
          online={online}
          initialLoginFilter={detectionsLogin}
        />
      )}
```

3. Заменить `onOpenDetections` у `ScreenshotsTab`:

```tsx
          onOpenDetections={(login) => {
            setDetectionsLogin(login);
            setTab('detections');
          }}
```

- [ ] **Step 3: Проверить тайпчек**

Run: `cd dashboard && npm run build`
Expected: сборка успешна.

- [ ] **Step 4: Commit**

```bash
git add dashboard/components/anticheat/detections-tab.tsx dashboard/app/anticheat/page.tsx
git commit -m "feat(dashboard): запрос скриншота из детектов и префильтр детектов по логину"
```

---

### Task 6: Финальная проверка

**Files:** нет новых.

- [ ] **Step 1: Полная сборка**

Run: `cd dashboard && npm run build`
Expected: успешно, ноль ошибок TS.

- [ ] **Step 2: Визуальная проверка через dev-стек**

Запустить `./dev.sh` (или `npm run dev:dashboard` при работающем бэкенде) и проверить руками:

1. Вкладка «Скриншоты»: галерея с миниатюрами, переключение на таблицу и обратно (выбор переживает перезагрузку страницы — localStorage).
2. Фильтры: поиск по логину, чипы статуса, пресеты даты, счётчик «показано N из M».
3. Клик по готовому скриншоту → лайтбокс: зум колесом к курсору, drag-пан, двойной клик fit⇄100%, кнопки +/−/вписать, листание ←/→ и стрелками по краям, счётчик кадров, Esc.
4. Скачивание — файл `login_YYYY-MM-DD_HH-mm.jpg`.
5. При онлайн-игроке: одиночный запрос → статус меняется живьём → просмотрщик открывается сам по готовности; «Скриншот у всех» → суммарный тост.
6. Из лайтбокса: «Детекты игрока» → вкладка «Детекты» с заполненным поиском; «Забанить» → confirm → бан.
7. Вкладка «Детекты»: кнопка камеры у онлайн-игрока, тост после запроса.

Без онлайн-игрока пункты 5–7 проверяются частично (кнопки скрыты/неактивны) — это ожидаемо.

- [ ] **Step 3: Итоговый коммит (если были правки по итогам проверки)**

```bash
git add -A dashboard && git commit -m "fix(dashboard): правки по итогам визуальной проверки скриншотов"
```
