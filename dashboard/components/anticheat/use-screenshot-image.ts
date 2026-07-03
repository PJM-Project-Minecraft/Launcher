'use client';

import { useEffect, useRef, useState } from 'react';
import { apiUrl, getToken } from '../../app/lib/api';

// Кэш objectURL изображений скриншотов. Живёт на модуль: не сбрасывается при
// смене фильтров/вида, очищается clearScreenshotImageCache() при уходе со вкладки.
const cache = new Map<string, Promise<string>>();

async function fetchImage(id: string): Promise<string> {
  const resp = await fetch(`${apiUrl}/api/admin/anticheat/screenshots/${id}/image`, {
    headers: { Authorization: `Bearer ${getToken()}` }
  });
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  const blob = await resp.blob();
  return URL.createObjectURL(blob);
}

/**
 * Загружает JPEG скриншота с Bearer-токеном (обычный img src не умеет
 * Authorization-заголовок) и отдаёт objectURL. enabled=false — не грузить
 * (ленивая загрузка: включается, когда карточка попала во вьюпорт).
 */
export function useScreenshotImage(id: string | null, enabled = true) {
  const [url, setUrl] = useState<string | null>(null);
  const [error, setError] = useState(false);

  useEffect(() => {
    if (!id || !enabled) return;
    let alive = true;
    let promise = cache.get(id);
    if (!promise) {
      promise = fetchImage(id);
      cache.set(id, promise);
      // Ошибку не кэшируем — следующий показ карточки повторит попытку.
      promise.catch(() => cache.delete(id));
    }
    promise.then(
      (u) => {
        if (alive) setUrl(u);
      },
      () => {
        if (alive) setError(true);
      }
    );
    return () => {
      alive = false;
    };
  }, [id, enabled]);

  return { url, error };
}

/** Revoke всех objectURL и очистка кэша — вызывается при размонтировании вкладки. */
export function clearScreenshotImageCache() {
  for (const promise of cache.values()) {
    void promise.then((u) => URL.revokeObjectURL(u)).catch(() => undefined);
  }
  cache.clear();
}

/** Однократный флаг «элемент попал во вьюпорт» (ленивая загрузка миниатюр). */
export function useInView<T extends Element>() {
  const ref = useRef<T | null>(null);
  const [inView, setInView] = useState(false);

  useEffect(() => {
    const el = ref.current;
    if (!el || inView) return;
    const obs = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) setInView(true);
      },
      { rootMargin: '200px' }
    );
    obs.observe(el);
    return () => obs.disconnect();
  }, [inView]);

  return { ref, inView };
}
