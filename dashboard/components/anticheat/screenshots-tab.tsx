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
