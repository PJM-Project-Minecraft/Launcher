'use client';

import { ShieldCheck } from 'lucide-react';
import { Card } from '../ui/card';
import { Badge } from '../ui/badge';
import { EmptyState } from '../ui/empty-state';
import type { Detection } from '../../app/lib/types';

function severityTone(severity: number): 'danger' | 'warn' | 'default' {
  if (severity >= 8) return 'danger';
  if (severity >= 5) return 'warn';
  return 'default';
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleString('ru-RU', {
    day: '2-digit',
    month: '2-digit',
    hour: '2-digit',
    minute: '2-digit'
  });
}

/** Лента последних детектов античита — единственный «живой» поток событий без нового API. */
export function EventsFeed({ detections }: { detections: Detection[] }) {
  return (
    <Card>
      <h2 className="mb-3 text-sm font-semibold text-fg">Последние детекты</h2>
      {detections.length === 0 ? (
        <EmptyState icon={ShieldCheck} title="Детектов нет" hint="Античит не зафиксировал нарушений." />
      ) : (
        <ul className="divide-y divide-edge/60">
          {detections.map((d) => (
            <li key={d.id} className="flex items-center gap-3 py-2.5 text-sm">
              <span className="min-w-0 truncate font-semibold text-fg">{d.login}</span>
              <span className="text-fg-faint">·</span>
              <span className="min-w-0 truncate text-fg-secondary">{d.type}</span>
              <Badge tone={severityTone(d.severity)}>{d.severity}</Badge>
              <span className="ml-auto shrink-0 text-xs text-fg-muted">{formatDate(d.createdAt)}</span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}
