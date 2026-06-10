'use client';

import { useState } from 'react';
import { ShieldCheck } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { Detection } from '../../app/lib/types';
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

export function DetectionsTab({
  detections,
  loading,
  onReload
}: {
  detections: Detection[];
  loading: boolean;
  onReload: () => Promise<void>;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const [banningId, setBanningId] = useState<string | null>(null);

  const stats = {
    total: detections.length,
    high: detections.filter((d) => d.severity >= 8).length,
    players: new Set(detections.map((d) => d.userUuid)).size
  };

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

  if (loading) {
    return <SkeletonTable rows={6} cols={7} />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="grid gap-3 sm:grid-cols-3">
        <StatCard label="Всего детектов" value={stats.total} hint="последние 200" />
        <StatCard label="Критичных" value={stats.high} tone={stats.high > 0 ? 'danger' : 'default'} hint="severity ≥ 8" />
        <StatCard label="Уникальных игроков" value={stats.players} />
      </div>

      {detections.length === 0 ? (
        <EmptyState icon={ShieldCheck} title="Детектов пока нет" hint="Античит не зафиксировал нарушений." />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Игрок</Th>
              <Th>Тип</Th>
              <Th>Сигнатура</Th>
              <Th>Источник</Th>
              <Th>Severity</Th>
              <Th>HWID</Th>
              <Th>Дата</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {detections.map((d) => (
              <tr key={d.id}>
                <Td className="font-medium">{d.login || d.userUuid}</Td>
                <Td className="text-fg-secondary">{d.type}</Td>
                <Td className="text-fg-secondary">{d.signature}</Td>
                <Td className="text-fg-muted">{d.source}</Td>
                <Td>
                  <Badge tone={severityTone(d.severity)}>{d.severity}</Badge>
                </Td>
                <Td className="font-mono text-xs text-fg-muted" title={d.hwidHash}>
                  {d.hwidHash ? `${d.hwidHash.slice(0, 12)}…` : '—'}
                </Td>
                <Td className="whitespace-nowrap text-fg-muted">{new Date(d.createdAt).toLocaleString('ru-RU')}</Td>
                <Td className="text-right">
                  <Button
                    variant="danger"
                    className="h-8 px-3"
                    loading={banningId === d.id}
                    onClick={() => void banAccount(d)}
                  >
                    Забанить
                  </Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </div>
  );
}
