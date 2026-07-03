'use client';

import { useEffect, useMemo, useState } from 'react';
import { Camera, ShieldCheck } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { Detection, DetectionStatus, OnlineSession } from '../../app/lib/types';
import { Input } from '../ui/input';
import { StatCard } from '../ui/stat-card';
import { Table, Th, Td } from '../ui/table';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { SkeletonTable } from '../ui/skeleton';
import { EmptyState } from '../ui/empty-state';
import { useToast } from '../ui/toast';
import { useConfirm } from '../ui/confirm';

function severityTone(severity: number): 'danger' | 'warn' | 'default' {
  if (severity >= 8) return 'danger';
  if (severity >= 5) return 'warn';
  return 'default';
}

function confidenceTone(confidence: string): 'danger' | 'default' {
  return confidence === 'hard' ? 'danger' : 'default';
}

const STATUS_LABELS: Record<string, string> = {
  new: 'Новый',
  reviewed: 'Просмотрен',
  confirmed: 'Подтверждён',
  dismissed: 'Отклонён'
};

function statusTone(status: string): 'danger' | 'warn' | 'ok' | 'default' {
  if (status === 'confirmed') return 'danger';
  if (status === 'new') return 'warn';
  if (status === 'dismissed') return 'ok';
  return 'default';
}

const selectClass =
  'rounded-lg border border-edge bg-transparent px-3 py-1.5 text-sm text-fg-secondary focus:border-fg-muted focus:outline-none';

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
  const toast = useToast();
  const confirm = useConfirm();
  const [banningId, setBanningId] = useState<string | null>(null);
  const [hwidBanningId, setHwidBanningId] = useState<string | null>(null);
  const [reviewingId, setReviewingId] = useState<string | null>(null);
  const [confidenceFilter, setConfidenceFilter] = useState('');
  const [statusFilter, setStatusFilter] = useState('');
  const [loginFilter, setLoginFilter] = useState(initialLoginFilter ?? '');
  const [shotRequestingId, setShotRequestingId] = useState<string | null>(null);

  // Префильтр из просмотрщика скриншотов: «Детекты игрока» обновляет проп.
  useEffect(() => {
    if (initialLoginFilter !== undefined && initialLoginFilter !== '') setLoginFilter(initialLoginFilter);
  }, [initialLoginFilter]);

  const stats = {
    total: detections.length,
    fresh: detections.filter((d) => d.status === 'new').length,
    hard: detections.filter((d) => d.confidence === 'hard').length,
    players: new Set(detections.map((d) => d.userUuid)).size
  };

  const filtered = useMemo(() => {
    const q = loginFilter.trim().toLowerCase();
    return detections.filter(
      (d) =>
        (confidenceFilter === '' || d.confidence === confidenceFilter) &&
        (statusFilter === '' || d.status === statusFilter) &&
        (q === '' || (d.login || d.userUuid).toLowerCase().includes(q))
    );
  }, [detections, confidenceFilter, statusFilter, loginFilter]);

  async function updateStatus(d: Detection, status: DetectionStatus) {
    setReviewingId(d.id);
    try {
      await api(`/api/admin/anticheat/detections/${d.id}`, { method: 'PATCH', body: { status } });
      toast('success', status === 'confirmed' ? 'Детект подтверждён' : 'Детект отклонён');
      await onReload();
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setReviewingId(null);
    }
  }

  async function banAccount(d: Detection) {
    const ok = await confirm({
      title: 'Забанить аккаунт',
      message: `Забанить аккаунт ${d.login || d.userUuid} по детекту «${d.signature}»?`,
      confirmLabel: 'Забанить',
      danger: true
    });
    if (!ok) return;
    setBanningId(d.id);
    try {
      await api('/api/admin/anticheat/bans/account', {
        method: 'POST',
        body: { userUuid: d.userUuid, login: d.login, reason: `manual: ${d.signature}` }
      });
      toast('success', `Аккаунт ${d.login || d.userUuid} забанен`);
      await onReload();
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setBanningId(null);
    }
  }

  async function banHwid(d: Detection) {
    if (!d.hwidHash) return;
    const ok = await confirm({
      title: 'HWID-бан',
      message: `Забанить устройство ${d.hwidHash.slice(0, 16)}… игрока ${d.login || d.userUuid} по детекту «${d.signature}»?`,
      confirmLabel: 'Забанить',
      danger: true
    });
    if (!ok) return;
    setHwidBanningId(d.id);
    try {
      await api('/api/admin/anticheat/bans/hwid', {
        method: 'POST',
        body: { hwidHash: d.hwidHash, reason: `manual: ${d.signature}` }
      });
      toast('success', 'HWID-бан выдан');
      await onReload();
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setHwidBanningId(null);
    }
  }

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

  if (loading) {
    return <SkeletonTable rows={6} cols={8} />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="grid gap-3 sm:grid-cols-4">
        <StatCard label="Всего детектов" value={stats.total} hint="последние 200" />
        <StatCard label="Не разобрано" value={stats.fresh} tone={stats.fresh > 0 ? 'warn' : 'default'} hint="статус new" />
        <StatCard label="Hard-сигналов" value={stats.hard} tone={stats.hard > 0 ? 'danger' : 'default'} hint="высокая уверенность" />
        <StatCard label="Уникальных игроков" value={stats.players} />
      </div>

      <div className="flex flex-wrap gap-2">
        <Input
          value={loginFilter}
          onChange={(e) => setLoginFilter(e.target.value)}
          placeholder="Поиск по логину…"
          className="h-9 w-44 text-sm"
          aria-label="Поиск по логину"
        />
        <select
          value={confidenceFilter}
          onChange={(e) => setConfidenceFilter(e.target.value)}
          className={selectClass}
          aria-label="Фильтр по уверенности"
        >
          <option value="">Вся уверенность</option>
          <option value="hard">Только hard</option>
          <option value="soft">Только soft</option>
        </select>
        <select
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
          className={selectClass}
          aria-label="Фильтр по статусу"
        >
          <option value="">Все статусы</option>
          <option value="new">Новые</option>
          <option value="confirmed">Подтверждённые</option>
          <option value="dismissed">Отклонённые</option>
        </select>
      </div>

      {filtered.length === 0 ? (
        <EmptyState icon={ShieldCheck} title="Детектов нет" hint="Под выбранные фильтры ничего не подходит." />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Игрок</Th>
              <Th>Тип</Th>
              <Th>Сигнатура</Th>
              <Th>Severity</Th>
              <Th>Статус</Th>
              <Th>HWID</Th>
              <Th>Дата</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {filtered.map((d) => (
              <tr key={d.id}>
                <Td className="font-medium">{d.login || d.userUuid}</Td>
                <Td className="text-fg-secondary" title={`источник: ${d.source}`}>
                  {d.type}
                </Td>
                <Td className="text-fg-secondary">{d.signature}</Td>
                <Td>
                  <div className="flex items-center gap-1.5">
                    <Badge tone={severityTone(d.severity)}>{d.severity}</Badge>
                    <Badge tone={confidenceTone(d.confidence)}>{d.confidence || '—'}</Badge>
                  </div>
                </Td>
                <Td>
                  <Badge tone={statusTone(d.status)}>{STATUS_LABELS[d.status] ?? d.status}</Badge>
                </Td>
                <Td className="font-mono text-xs text-fg-muted" title={d.hwidHash}>
                  {d.hwidHash ? `${d.hwidHash.slice(0, 12)}…` : '—'}
                </Td>
                <Td className="whitespace-nowrap text-fg-muted">{new Date(d.createdAt).toLocaleString('ru-RU')}</Td>
                <Td className="text-right">
                  <div className="flex justify-end gap-2">
                    {d.status === 'new' && (
                      <>
                        <Button
                          variant="ghost"
                          className="h-8 px-3"
                          loading={reviewingId === d.id}
                          onClick={() => void updateStatus(d, 'confirmed')}
                        >
                          Подтвердить
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-8 px-3"
                          loading={reviewingId === d.id}
                          onClick={() => void updateStatus(d, 'dismissed')}
                        >
                          Отклонить
                        </Button>
                      </>
                    )}
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
                    <Button
                      variant="danger"
                      className="h-8 px-3"
                      loading={banningId === d.id}
                      onClick={() => void banAccount(d)}
                    >
                      Бан
                    </Button>
                    {d.hwidHash && (
                      <Button
                        variant="ghost"
                        className="h-8 px-3"
                        loading={hwidBanningId === d.id}
                        onClick={() => void banHwid(d)}
                      >
                        HWID
                      </Button>
                    )}
                  </div>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </div>
  );
}
