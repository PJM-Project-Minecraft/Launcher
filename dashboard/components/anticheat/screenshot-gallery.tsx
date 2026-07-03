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
