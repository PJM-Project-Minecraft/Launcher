'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { useRouter } from 'next/navigation';
import { AnimatePresence, motion } from 'framer-motion';
import { CornerDownLeft, Search, User } from 'lucide-react';
import { api } from '../../app/lib/api';
import type { AdminUser } from '../../app/lib/types';
import { navItems } from './sidebar';

type Item = { key: string; label: string; hint?: string; icon: 'nav' | 'user'; action: () => void };

/** Командная палитра (Ctrl+K): переходы по разделам + поиск игроков по нику. */
export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const router = useRouter();
  const [query, setQuery] = useState('');
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [cursor, setCursor] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) {
      setQuery('');
      setUsers([]);
      setCursor(0);
      setTimeout(() => inputRef.current?.focus(), 30);
    }
  }, [open]);

  // Поиск игроков с debounce.
  useEffect(() => {
    if (!open || query.trim().length < 2) {
      setUsers([]);
      return;
    }
    const timer = setTimeout(() => {
      api<{ items: AdminUser[] }>(`/api/admin/users?q=${encodeURIComponent(query.trim())}`)
        .then((res) => setUsers((res.items ?? []).slice(0, 6)))
        .catch(() => setUsers([]));
    }, 250);
    return () => clearTimeout(timer);
  }, [query, open]);

  const items = useMemo<Item[]>(() => {
    const go = (href: string) => () => {
      router.push(href);
      onClose();
    };
    const q = query.trim().toLowerCase();
    const nav = navItems
      .filter((item) => !q || item.label.toLowerCase().includes(q))
      .map((item) => ({
        key: `nav:${item.href}`,
        label: item.label,
        hint: 'Перейти',
        icon: 'nav' as const,
        action: go(item.href)
      }));
    const found = users.map((user) => ({
      key: `user:${user.id}`,
      label: user.login,
      hint: user.isBanned ? 'Игрок · забанен' : 'Игрок',
      icon: 'user' as const,
      action: go(`/users?q=${encodeURIComponent(user.login)}`)
    }));
    return [...nav, ...found];
  }, [query, users, router, onClose]);

  useEffect(() => setCursor(0), [items.length]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setCursor((c) => Math.min(c + 1, items.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setCursor((c) => Math.max(c - 1, 0));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      items[cursor]?.action();
    } else if (e.key === 'Escape') {
      onClose();
    }
  };

  return (
    <AnimatePresence>
      {open && (
        <motion.div
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          className="fixed inset-0 z-50 flex items-start justify-center bg-black/60 p-4 pt-[12vh] backdrop-blur-sm"
          onClick={onClose}
        >
          <motion.div
            initial={{ opacity: 0, scale: 0.97, y: -8 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.97, y: -8 }}
            transition={{ duration: 0.15 }}
            className="w-full max-w-lg overflow-hidden rounded-xl border border-edge bg-bg/95 shadow-2xl backdrop-blur-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex items-center gap-2.5 border-b border-edge px-4">
              <Search size={16} className="text-fg-faint" />
              <input
                ref={inputRef}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={onKeyDown}
                placeholder="Раздел или ник игрока…"
                className="h-12 w-full bg-transparent text-sm outline-none placeholder:text-fg-faint"
              />
            </div>
            <div className="max-h-72 overflow-y-auto p-2">
              {items.length === 0 && <div className="px-3 py-6 text-center text-sm text-fg-faint">Ничего не найдено</div>}
              {items.map((item, index) => {
                const Icon = item.icon === 'user' ? User : CornerDownLeft;
                return (
                  <button
                    key={item.key}
                    onClick={item.action}
                    onMouseEnter={() => setCursor(index)}
                    className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left text-sm transition ${
                      index === cursor ? 'bg-surface-strong text-fg' : 'text-fg-secondary'
                    }`}
                  >
                    <Icon size={14} className="shrink-0 text-fg-faint" />
                    <span className="flex-1">{item.label}</span>
                    {item.hint && <span className="text-xs text-fg-faint">{item.hint}</span>}
                  </button>
                );
              })}
            </div>
          </motion.div>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
