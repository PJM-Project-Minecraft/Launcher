'use client';

// Секция «Сборка»: степпер Профиль → Клиент → Manifest и действия prepare/scan.

import { useEffect, useState } from 'react';
import { Check, Download, FileSearch, Loader2, Lock } from 'lucide-react';
import { Button } from '../ui/button';
import { useToast } from '../ui/toast';
import { api, errorMessage } from '../../app/lib/api';
import type { Profile } from '../../app/lib/types';
import { formatBytes, formatDate } from './helpers';

type StepState = 'done' | 'active' | 'loading' | 'locked';

function Step({ index, label, state, last }: { index: number; label: string; state: StepState; last?: boolean }) {
  const circle =
    state === 'done'
      ? 'border-ok/40 bg-ok/10 text-ok'
      : state === 'loading'
        ? 'border-warn/40 bg-warn/10 text-warn'
        : state === 'active'
          ? 'border-edge-strong bg-surface-strong text-fg'
          : 'border-edge bg-surface text-fg-faint';
  return (
    <div className={`flex items-center ${last ? '' : 'flex-1'}`}>
      <div className="flex items-center gap-2.5">
        <span className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-full border ${circle}`}>
          {state === 'done' && <Check size={15} />}
          {state === 'loading' && <Loader2 size={15} className="animate-spin" />}
          {state === 'locked' && <Lock size={13} />}
          {state === 'active' && <span className="text-xs font-semibold">{index}</span>}
        </span>
        <span className={`text-sm font-medium ${state === 'locked' ? 'text-fg-faint' : 'text-fg'}`}>{label}</span>
      </div>
      {!last && <div className="mx-3 h-px flex-1 bg-edge" />}
    </div>
  );
}

export function ClientFlow({
  profile,
  onRefresh
}: {
  profile: Profile | null;
  onRefresh: () => Promise<void>;
}) {
  const toast = useToast();
  const [isScanning, setIsScanning] = useState(false);
  const [isPreparingLocal, setIsPreparingLocal] = useState(false);

  const isPreparing = isPreparingLocal || profile?.clientStatus === 'preparing';
  const clientReady = Boolean(profile?.clientPrepared);
  const hasManifest = clientReady && (profile?.fileCount ?? 0) > 0;

  // Поллинг статуса, пока идёт подготовка или скан (на случай запуска из другой сессии).
  useEffect(() => {
    if (!isPreparing && !isScanning) {
      return;
    }
    const interval = window.setInterval(() => void onRefresh(), 4000);
    return () => window.clearInterval(interval);
  }, [isPreparing, isScanning, onRefresh]);

  const profileStep: StepState = profile ? 'done' : 'active';
  const clientStep: StepState = !profile
    ? 'locked'
    : isPreparing
      ? 'loading'
      : clientReady
        ? 'done'
        : 'active';
  const manifestStep: StepState = !clientReady
    ? 'locked'
    : isScanning
      ? 'loading'
      : hasManifest
        ? 'done'
        : 'active';

  async function prepareClient() {
    if (!profile) return;
    setIsPreparingLocal(true);
    toast('info', `Файлы клиента скачиваются для профиля «${profile.name}». Это может занять несколько минут.`);
    try {
      const result = await api<{ fileCount: number; totalSize: number; downloaded: number; message: string }>(
        `/api/admin/profiles/${profile.id}/prepare-client`,
        { method: 'POST' }
      );
      await onRefresh();
      toast(
        'success',
        `${result.message} Скачано/обновлено: ${result.downloaded}; manifest: ${result.fileCount} файлов, ${formatBytes(result.totalSize)}`
      );
    } catch (error) {
      toast('error', errorMessage(error));
    } finally {
      setIsPreparingLocal(false);
    }
  }

  async function scanProfile() {
    if (!profile) return;
    setIsScanning(true);
    try {
      const result = await api<{ fileCount: number; totalSize: number }>(
        `/api/admin/profiles/${profile.id}/scan`,
        { method: 'POST' }
      );
      await onRefresh();
      toast('success', `Manifest собран: ${result.fileCount} файлов, ${formatBytes(result.totalSize)}`);
    } catch (error) {
      toast('error', errorMessage(error));
    } finally {
      setIsScanning(false);
    }
  }

  return (
    <div className="flex flex-col gap-5">
      <div className="flex items-center rounded-xl border border-edge bg-surface px-4 py-4">
        <Step index={1} label="Профиль" state={profileStep} />
        <Step index={2} label="Клиент" state={clientStep} />
        <Step index={3} label="Manifest" state={manifestStep} last />
      </div>

      {!profile && (
        <p className="text-sm text-fg-muted">Сначала сохрани профиль — после этого можно собирать клиент и manifest.</p>
      )}

      {profile && (
        <div className="grid grid-cols-2 gap-3 max-md:grid-cols-1">
          <div className="rounded-lg border border-edge bg-surface px-3 py-2.5">
            <div className="text-xs text-fg-faint">Папка на backend</div>
            <div className="truncate text-sm text-fg-secondary">storage/profiles/{profile.slug}/files</div>
          </div>
          <div className="rounded-lg border border-edge bg-surface px-3 py-2.5">
            <div className="text-xs text-fg-faint">Моды клиента</div>
            <div className="truncate text-sm text-fg-secondary">storage/profiles/{profile.slug}/files/mods</div>
          </div>
          <div className="rounded-lg border border-edge bg-surface px-3 py-2.5">
            <div className="text-xs text-fg-faint">Manifest</div>
            <div className="text-sm text-fg-secondary">
              {profile.fileCount} файлов · {formatBytes(profile.totalSize)}
            </div>
          </div>
          <div className="rounded-lg border border-edge bg-surface px-3 py-2.5">
            <div className="text-xs text-fg-faint">Обновлено</div>
            <div className="text-sm text-fg-secondary">
              {profile.manifestUpdatedAt ? formatDate(profile.manifestUpdatedAt) : '—'}
            </div>
          </div>
        </div>
      )}

      <div className="flex flex-wrap gap-2">
        <Button variant="ghost" loading={isPreparingLocal} disabled={!profile || isPreparing} onClick={() => void prepareClient()}>
          {!isPreparingLocal && <Download size={15} />}
          {isPreparing ? 'Собирается…' : clientReady ? 'Собрать клиент заново' : 'Собрать клиент'}
        </Button>
        <Button variant="ghost" loading={isScanning} disabled={!profile || !clientReady || isScanning} onClick={() => void scanProfile()}>
          {!isScanning && <FileSearch size={15} />}
          {clientReady ? 'Сканировать файлы' : 'Сначала собери клиент'}
        </Button>
      </div>
      <p className="text-xs text-fg-faint">
        Кидай моды по SFTP в папку files/mods, затем сканируй файлы — manifest откроет их для скачивания лаунчером.
      </p>
    </div>
  );
}
