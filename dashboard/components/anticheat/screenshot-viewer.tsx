'use client';

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Camera,
  ChevronLeft,
  ChevronRight,
  Download,
  Maximize,
  Minus,
  Plus,
  ShieldAlert,
  Ban,
  X
} from 'lucide-react';
import type { Screenshot } from '../../app/lib/types';
import { Spinner } from '../ui/spinner';
import { useScreenshotImage } from './use-screenshot-image';

type Transform = { scale: number; x: number; y: number };
const FIT: Transform = { scale: 1, x: 0, y: 0 };
const MAX_SCALE = 8;

/** Кнопка тулбара на тёмном оверлее (стили Button заточены под светлую тему). */
function TBtn({
  title,
  onClick,
  danger = false,
  children
}: {
  title: string;
  onClick: () => void;
  danger?: boolean;
  children: React.ReactNode;
}) {
  return (
    <button
      title={title}
      aria-label={title}
      onClick={onClick}
      className={`inline-flex h-9 w-9 items-center justify-center rounded-lg border border-white/15 bg-white/5 transition hover:bg-white/15 ${
        danger ? 'text-red-400' : 'text-white'
      }`}
    >
      {children}
    </button>
  );
}

function downloadName(s: Screenshot): string {
  const d = new Date(s.createdAt);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${s.login || s.userUuid}_${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}_${pad(
    d.getHours()
  )}-${pad(d.getMinutes())}.jpg`;
}

/**
 * Полноэкранный просмотрщик: зум колесом к курсору, drag-пан, двойной клик
 * fit⇄100%, листание ←/→ по отфильтрованным done-скриншотам, скачивание,
 * действия по игроку. CSS transform, без зависимостей.
 */
export function ScreenshotViewer({
  items,
  index,
  onNavigate,
  onClose,
  canRequestMore,
  onRequestMore,
  onGoToDetections,
  onBan
}: {
  items: Screenshot[];
  index: number;
  onNavigate: (index: number) => void;
  onClose: () => void;
  canRequestMore: boolean;
  onRequestMore: () => void;
  onGoToDetections: () => void;
  onBan: () => void;
}) {
  const shot = items[index] ?? null;
  const { url, error } = useScreenshotImage(shot?.id ?? null);
  // Прелоад соседей — мгновенное листание.
  useScreenshotImage(items[index - 1]?.id ?? null);
  useScreenshotImage(items[index + 1]?.id ?? null);

  const [t, setT] = useState<Transform>(FIT);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const imgRef = useRef<HTMLImageElement | null>(null);
  const drag = useRef<{ pointerId: number; startX: number; startY: number; baseX: number; baseY: number } | null>(
    null
  );

  // Сброс зума при смене кадра.
  useEffect(() => setT(FIT), [index]);

  // Блокируем прокрутку страницы под оверлеем (заодно снимает проблему
  // passive wheel-листенеров React — preventDefault не нужен).
  useEffect(() => {
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = prev;
    };
  }, []);

  const goPrev = useCallback(() => {
    if (index > 0) onNavigate(index - 1);
  }, [index, onNavigate]);
  const goNext = useCallback(() => {
    if (index < items.length - 1) onNavigate(index + 1);
  }, [index, items.length, onNavigate]);

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose();
      else if (e.key === 'ArrowLeft') goPrev();
      else if (e.key === 'ArrowRight') goNext();
    }
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose, goPrev, goNext]);

  // Зум так, чтобы точка под курсором осталась на месте.
  // Экранная точка кадра: p = t + scale * p_img  ⇒  t' = c − k·(c − t), k = scale'/scale.
  const zoomAt = useCallback((clientX: number, clientY: number, factor: number) => {
    const rect = containerRef.current?.getBoundingClientRect();
    if (!rect) return;
    const cx = clientX - rect.left - rect.width / 2;
    const cy = clientY - rect.top - rect.height / 2;
    setT((prev) => {
      const scale = Math.min(MAX_SCALE, Math.max(1, prev.scale * factor));
      if (scale === prev.scale) return prev;
      if (scale === 1) return FIT;
      const k = scale / prev.scale;
      return { scale, x: cx - k * (cx - prev.x), y: cy - k * (cy - prev.y) };
    });
  }, []);

  function zoomCenter(factor: number) {
    const rect = containerRef.current?.getBoundingClientRect();
    if (!rect) return;
    zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, factor);
  }

  function onWheel(e: React.WheelEvent) {
    zoomAt(e.clientX, e.clientY, e.deltaY < 0 ? 1.2 : 1 / 1.2);
  }

  function onDoubleClick(e: React.MouseEvent) {
    if (t.scale > 1) {
      setT(FIT);
      return;
    }
    // fit → 100%: во сколько раз натуральный размер больше вписанного.
    const img = imgRef.current;
    if (!img || !img.naturalWidth) return;
    const renderedWidth = img.getBoundingClientRect().width;
    if (renderedWidth <= 0) return;
    zoomAt(e.clientX, e.clientY, Math.max(1.01, img.naturalWidth / renderedWidth));
  }

  function onPointerDown(e: React.PointerEvent) {
    if (t.scale <= 1) return;
    drag.current = { pointerId: e.pointerId, startX: e.clientX, startY: e.clientY, baseX: t.x, baseY: t.y };
    (e.target as Element).setPointerCapture(e.pointerId);
  }

  function onPointerMove(e: React.PointerEvent) {
    const d = drag.current;
    if (!d || d.pointerId !== e.pointerId) return;
    setT((prev) => ({ ...prev, x: d.baseX + (e.clientX - d.startX), y: d.baseY + (e.clientY - d.startY) }));
  }

  function onPointerUp(e: React.PointerEvent) {
    if (drag.current?.pointerId === e.pointerId) drag.current = null;
  }

  function download() {
    if (!url || !shot) return;
    const a = document.createElement('a');
    a.href = url;
    a.download = downloadName(shot);
    a.click();
  }

  if (!shot) return null;

  return (
    <div className="fixed inset-0 z-50 flex flex-col bg-black/90 backdrop-blur-sm">
      {/* Тулбар */}
      <div className="flex items-center gap-2 border-b border-white/10 px-4 py-2.5">
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-semibold text-white">{shot.login || shot.userUuid}</div>
          <div className="truncate text-xs text-white/60">
            {new Date(shot.createdAt).toLocaleString('ru-RU')} · {shot.width}×{shot.height} ·{' '}
            {(shot.size / 1024).toFixed(0)} КБ
            {shot.requestedBy ? ` · запросил ${shot.requestedBy}` : ''}
          </div>
        </div>
        <span className="whitespace-nowrap text-xs text-white/60">
          {index + 1} / {items.length}
        </span>
        <TBtn title="Отдалить" onClick={() => zoomCenter(1 / 1.4)}>
          <Minus size={16} />
        </TBtn>
        <TBtn title="Приблизить" onClick={() => zoomCenter(1.4)}>
          <Plus size={16} />
        </TBtn>
        <TBtn title="Вписать в экран" onClick={() => setT(FIT)}>
          <Maximize size={16} />
        </TBtn>
        <TBtn title="Скачать JPEG" onClick={download}>
          <Download size={16} />
        </TBtn>
        {canRequestMore && (
          <TBtn title="Запросить ещё скриншот" onClick={onRequestMore}>
            <Camera size={16} />
          </TBtn>
        )}
        <TBtn title="Детекты игрока" onClick={onGoToDetections}>
          <ShieldAlert size={16} />
        </TBtn>
        <TBtn title="Забанить аккаунт" onClick={onBan} danger>
          <Ban size={16} />
        </TBtn>
        <TBtn title="Закрыть (Esc)" onClick={onClose}>
          <X size={16} />
        </TBtn>
      </div>

      {/* Сцена */}
      <div
        ref={containerRef}
        className="relative flex-1 touch-none overflow-hidden"
        onWheel={onWheel}
        onDoubleClick={onDoubleClick}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
      >
        <div className="flex h-full w-full items-center justify-center">
          {url ? (
            // eslint-disable-next-line @next/next/no-img-element
            <img
              ref={imgRef}
              src={url}
              alt={`Скриншот ${shot.login || shot.userUuid}`}
              draggable={false}
              style={{ transform: `translate(${t.x}px, ${t.y}px) scale(${t.scale})` }}
              className={`max-h-full max-w-full select-none ${t.scale > 1 ? 'cursor-grab' : 'cursor-zoom-in'}`}
            />
          ) : error ? (
            <span className="text-sm text-white/70">Не удалось загрузить изображение</span>
          ) : (
            <Spinner size={24} />
          )}
        </div>

        {index > 0 && (
          <button
            aria-label="Предыдущий"
            onClick={goPrev}
            className="absolute left-3 top-1/2 -translate-y-1/2 rounded-full border border-white/15 bg-black/50 p-2 text-white transition hover:bg-black/80"
          >
            <ChevronLeft size={22} />
          </button>
        )}
        {index < items.length - 1 && (
          <button
            aria-label="Следующий"
            onClick={goNext}
            className="absolute right-3 top-1/2 -translate-y-1/2 rounded-full border border-white/15 bg-black/50 p-2 text-white transition hover:bg-black/80"
          >
            <ChevronRight size={22} />
          </button>
        )}
      </div>
    </div>
  );
}
