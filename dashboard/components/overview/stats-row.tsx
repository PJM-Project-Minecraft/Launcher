'use client';

import { StatCard } from '../ui/stat-card';
import type { Stats } from '../../app/lib/types';

/** Верхняя сетка метрик обзора: аккаунты, баны, входы за сутки и статус API. */
export function StatsRow({ stats, apiOnline }: { stats: Stats; apiOnline: boolean | null }) {
  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <StatCard label="Аккаунты" value={stats.totalUsers} />
      <StatCard label="Баны" value={stats.bannedUsers} tone={stats.bannedUsers > 0 ? 'danger' : 'default'} />
      <StatCard
        label="Входы 24ч"
        value={stats.authSuccess24h}
        hint={`${stats.authFailure24h} неудачных`}
        tone={stats.authFailure24h > 0 ? 'warn' : 'default'}
      />
      <StatCard
        label="API"
        value={apiOnline === null ? '…' : apiOnline ? 'Online' : 'Offline'}
        tone={apiOnline === null ? 'default' : apiOnline ? 'ok' : 'danger'}
      />
    </div>
  );
}
