'use client';

import { useCallback, useEffect, useState } from 'react';
import { Camera, Monitor, RefreshCw, ImageIcon } from 'lucide-react';
import { api, apiUrl, errorMessage, getToken } from '../../app/lib/api';
import type { OnlineSession, Screenshot } from '../../app/lib/types';
import { Table, Th, Td } from '../ui/table';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { SkeletonTable } from '../ui/skeleton';
import { EmptyState } from '../ui/empty-state';
import { Modal } from '../ui/modal';
import { useToast } from '../ui/toast';

const STATUS_LABELS: Record<string, string> = {
  pending: 'Ожидание',
  capturing: 'Захват',
  done: 'Готов',
  failed: 'Ошибка'
};

function statusTone(status: string): 'ok' | 'warn' | 'danger' | 'default' {
  if (status === 'done') return 'ok';
  if (status === 'failed') return 'danger';
  if (status === 'pending' || status === 'capturing') return 'warn';
  return 'default';
}

export function ScreenshotsTab({
  screenshots,
  loading,
  onReload
}: {
  screenshots: Screenshot[];
  loading: boolean;
  onReload: () => Promise<void>;
}) {
  const toast = useToast();
  const [online, setOnline] = useState<OnlineSession[]>([]);
  const [onlineLoading, setOnlineLoading] = useState(false);
  const [requesting, setRequesting] = useState<string | null>(null); // nonce
  const [viewing, setViewing] = useState<Screenshot | null>(null);
  const [imgUrl, setImgUrl] = useState<string | null>(null);

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
    void reloadOnline();
  }, [reloadOnline]);

  // Поллинг: пока есть pending/capturing скриншоты, обновляем каждые 2с.
  useEffect(() => {
    const hasPending = screenshots.some((s) => s.status === 'pending' || s.status === 'capturing');
    if (!hasPending) return;
    const id = setInterval(() => void onReload(), 2000);
    return () => clearInterval(id);
  }, [screenshots, onReload]);

  // Обновляем онлайн-список при появлении нового done-скриншота (игрок мог выйти).
  useEffect(() => {
    void reloadOnline();
  }, [reloadOnline, screenshots.length]);

  async function requestShot(sess: OnlineSession) {
    setRequesting(sess.nonce);
    try {
      await api('/api/admin/anticheat/screenshots', {
        method: 'POST',
        body: { nonce: sess.nonce }
      });
      toast('success', `Запрос скриншота отправлен: ${sess.login}`);
      await Promise.all([onReload(), reloadOnline()]);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setRequesting(null);
    }
  }

  // Загрузка изображения с JWT-заголовком (img src не поддерживает Bearer).
  async function openViewer(s: Screenshot) {
    setViewing(s);
    setImgUrl(null);
    try {
      const resp = await fetch(`${apiUrl}/api/admin/anticheat/screenshots/${s.id}/image`, {
        headers: { Authorization: `Bearer ${getToken()}` }
      });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const blob = await resp.blob();
      setImgUrl(URL.createObjectURL(blob));
    } catch (e) {
      toast('error', 'Не удалось загрузить изображение: ' + errorMessage(e));
    }
  }

  function closeViewer() {
    if (imgUrl) URL.revokeObjectURL(imgUrl);
    setImgUrl(null);
    setViewing(null);
  }

  useEffect(() => {
    return () => {
      if (imgUrl) URL.revokeObjectURL(imgUrl);
    };
  }, [imgUrl]);

  return (
    <div className="flex flex-col gap-4">
      {/* Онлайн-игроки */}
      <div className="rounded-xl border border-edge bg-bg/50 p-4">
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold">Онлайн-игроки ({online.length})</h3>
          <Button variant="ghost" className="h-8 px-3" loading={onlineLoading} onClick={() => void reloadOnline()}>
            <RefreshCw size={14} />
            <span className="ml-1.5">Обновить</span>
          </Button>
        </div>
        {online.length === 0 ? (
          <p className="py-4 text-center text-sm text-fg-muted">Нет игроков онлайн</p>
        ) : (
          <div className="flex flex-wrap gap-2">
            {online.map((s) => (
              <div
                key={s.nonce}
                className="flex items-center gap-2 rounded-lg border border-edge bg-bg px-3 py-2"
              >
                <Monitor size={16} className="text-fg-muted" />
                <span className="text-sm font-medium">{s.login}</span>
                {s.ipAddress && (
                  <span className="font-mono text-xs text-fg-muted">{s.ipAddress}</span>
                )}
                <Button
                  variant="primary"
                  className="h-7 px-2.5"
                  loading={requesting === s.nonce}
                  onClick={() => void requestShot(s)}
                >
                  <Camera size={14} />
                  <span className="ml-1">Скриншот</span>
                </Button>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* История скриншотов */}
      {loading ? (
        <SkeletonTable rows={5} cols={6} />
      ) : screenshots.length === 0 ? (
        <EmptyState icon={ImageIcon} title="Скриншотов нет" hint="Выберите онлайн-игрока и запросите скриншот экрана." />
      ) : (
        <Table>
          <thead>
            <tr>
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
                <Td className="font-medium">{s.login || s.userUuid}</Td>
                <Td>
                  <Badge tone={statusTone(s.status)}>{STATUS_LABELS[s.status] ?? s.status}</Badge>
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
                    <Button variant="ghost" className="h-8 px-3" onClick={() => void openViewer(s)}>
                      Посмотреть
                    </Button>
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      <Modal open={!!viewing} onClose={closeViewer} title={viewing ? `Скриншот: ${viewing.login}` : ''} wide>
        {viewing && (
          <div className="flex flex-col gap-3">
            <div className="text-sm text-fg-secondary">
              {viewing.width}×{viewing.height} · {new Date(viewing.createdAt).toLocaleString('ru-RU')}
            </div>
            {imgUrl ? (
              <img src={imgUrl} alt="Скриншот экрана игрока" className="w-full rounded-lg border border-edge" />
            ) : (
              <div className="flex h-48 items-center justify-center text-sm text-fg-muted">Загрузка…</div>
            )}
          </div>
        )}
      </Modal>
    </div>
  );
}
