'use client';

import { useEffect, useRef } from 'react';
import { apiUrl } from './api';

/**
 * Подписка на SSE-события профилей backend'а; onEvent зовётся на каждое сообщение.
 * Реконнект встроен в EventSource; при размонтировании соединение закрывается.
 */
export function useProfileEvents(onEvent: () => void) {
  const handler = useRef(onEvent);
  handler.current = onEvent;
  useEffect(() => {
    const source = new EventSource(`${apiUrl}/api/profiles/events`);
    source.onmessage = () => handler.current();
    return () => source.close();
  }, []);
}
