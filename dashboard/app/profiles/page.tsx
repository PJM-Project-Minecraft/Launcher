'use client';

// Страница профилей: грузит список, держит выбор и раскладку «список / редактор».

import { useCallback, useEffect, useState } from 'react';
import { Package } from 'lucide-react';
import { Button } from '../../components/ui/button';
import { EmptyState } from '../../components/ui/empty-state';
import { Skeleton, SkeletonTable } from '../../components/ui/skeleton';
import { useToast } from '../../components/ui/toast';
import { api, errorMessage } from '../lib/api';
import { useProfileEvents } from '../lib/use-sse';
import type { Profile } from '../lib/types';
import { ProfileList } from '../../components/profiles/profile-list';
import { ProfileEditor } from '../../components/profiles/profile-editor';

export default function ProfilesPage() {
  const toast = useToast();
  const [profiles, setProfiles] = useState<Profile[] | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [isCreating, setIsCreating] = useState(false);

  const load = useCallback(
    async (silent = false) => {
      try {
        const data = await api<Profile[]>('/api/admin/profiles');
        setProfiles(data);
        setSelectedId((current) => current ?? data[0]?.id ?? null);
      } catch (error) {
        if (!silent) toast('error', errorMessage(error));
      }
    },
    [toast]
  );

  useEffect(() => {
    void load();
  }, [load]);

  // SSE backend'а: тихо обновляем список при любом событии профилей.
  useProfileEvents(() => void load(true));

  if (profiles === null) {
    return (
      <div className="grid items-start gap-5 lg:grid-cols-[300px_1fr]">
        <div className="flex flex-col gap-3">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </div>
        <SkeletonTable rows={8} cols={2} />
      </div>
    );
  }

  if (profiles.length === 0 && !isCreating) {
    return (
      <EmptyState
        icon={Package}
        title="Пока нет ни одного профиля"
        hint="Профиль описывает сборку Minecraft: версию, загрузчик, Java и команды запуска."
        action={
          <Button variant="primary" onClick={() => setIsCreating(true)}>
            Создать профиль
          </Button>
        }
      />
    );
  }

  const selected = isCreating ? null : profiles.find((profile) => profile.id === selectedId) ?? null;

  return (
    <div className="grid items-start gap-5 lg:grid-cols-[300px_1fr]">
      <ProfileList
        profiles={profiles}
        selectedId={isCreating ? null : selectedId}
        onSelect={(id) => {
          setIsCreating(false);
          setSelectedId(id);
        }}
        onCreate={() => setIsCreating(true)}
      />
      <ProfileEditor
        key={selected?.id ?? 'new'}
        profile={selected}
        onSaved={(profile) => {
          setIsCreating(false);
          setSelectedId(profile.id);
          void load(true);
        }}
        onDeleted={() => {
          setSelectedId(null);
          setIsCreating(false);
          void load(true);
        }}
        onRefresh={() => load(true)}
      />
    </div>
  );
}
