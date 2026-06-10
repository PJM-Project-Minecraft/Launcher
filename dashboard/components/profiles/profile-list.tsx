'use client';

// Левая колонка: список профилей с бейджем статуса клиента и кнопкой создания.

import { Plus } from 'lucide-react';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import type { Profile } from '../../app/lib/types';
import { formatBytes } from './helpers';

function clientBadge(profile: Profile) {
  if (profile.clientPrepared) {
    return <Badge tone="ok">Готов</Badge>;
  }
  if (profile.clientStatus === 'preparing') {
    return <Badge tone="warn">Сборка…</Badge>;
  }
  return <Badge>Не собран</Badge>;
}

export function ProfileList({
  profiles,
  selectedId,
  onSelect,
  onCreate
}: {
  profiles: Profile[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  onCreate: () => void;
}) {
  return (
    <div className="flex flex-col gap-3">
      <Button variant="primary" onClick={onCreate}>
        <Plus size={16} />
        Новый профиль
      </Button>

      <div className="flex flex-col gap-2">
        {profiles.map((profile) => {
          const isActive = profile.id === selectedId;
          return (
            <button
              key={profile.id}
              type="button"
              onClick={() => onSelect(profile.id)}
              className={`w-full rounded-xl border p-4 text-left transition ${
                isActive
                  ? 'border-edge-strong bg-surface-strong'
                  : 'border-edge bg-surface hover:bg-surface-strong'
              }`}
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <div className="truncate text-sm font-semibold text-fg">{profile.name}</div>
                  <div className="truncate text-xs text-fg-faint">{profile.slug}</div>
                </div>
                {clientBadge(profile)}
              </div>
              <div className="mt-2.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
                <span>
                  {profile.gameVersion}
                  {profile.loader !== 'vanilla' && ` · ${profile.loader}`}
                </span>
                <span>
                  {profile.fileCount} файлов · {formatBytes(profile.totalSize)}
                </span>
              </div>
            </button>
          );
        })}
      </div>
    </div>
  );
}
