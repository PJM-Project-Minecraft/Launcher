'use client';

import { useCallback, useEffect, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import { ChevronLeft, ChevronRight, Users } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { AdminUser } from '../../app/lib/types';
import { Badge } from '../ui/badge';
import { IconButton } from '../ui/button';
import { EmptyState } from '../ui/empty-state';
import { Input } from '../ui/input';
import { SkeletonTable } from '../ui/skeleton';
import { ClickableRow, Table, Td, Th } from '../ui/table';
import { useToast } from '../ui/toast';
import { UserDetailPanel } from './user-detail-panel';

const PAGE_SIZE = 30;

function fmtDate(value?: string | null) {
  return value ? new Date(value).toLocaleString('ru-RU') : '—';
}

/** Список пользователей: поиск с debounce, таблица, панель деталей справа. */
export function UsersTable() {
  const toast = useToast();
  // Начальный q — переход из командной палитры (/users?q=<login>).
  const initialQ = useSearchParams().get('q') ?? '';

  const [query, setQuery] = useState(initialQ);
  const [debouncedQ, setDebouncedQ] = useState(initialQ.trim());
  const [page, setPage] = useState(1);
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  useEffect(() => {
    const timer = setTimeout(() => {
      setDebouncedQ(query.trim());
      setPage(1);
    }, 300);
    return () => clearTimeout(timer);
  }, [query]);

  const loadUsers = useCallback(async () => {
    try {
      const res = await api<{ items: AdminUser[]; total: number }>(
        `/api/admin/users?q=${encodeURIComponent(debouncedQ)}&page=${page}`
      );
      setUsers(res.items ?? []);
      setTotal(res.total ?? 0);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [debouncedQ, page, toast]);

  useEffect(() => {
    void loadUsers();
  }, [loadUsers]);

  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  return (
    <div className="grid items-start gap-5 lg:grid-cols-[1fr_380px]">
      <div className="flex min-w-0 flex-col gap-4">
        <Input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Поиск: логин, e-mail, UUID"
        />

        {loading ? (
          <SkeletonTable rows={8} cols={5} />
        ) : users.length === 0 ? (
          <EmptyState
            icon={Users}
            title="Ничего не найдено"
            hint={debouncedQ ? `По запросу «${debouncedQ}» пользователей нет.` : 'Пользователей пока нет.'}
          />
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>Логин</Th>
                <Th>Email</Th>
                <Th>Роль</Th>
                <Th>Флаги</Th>
                <Th>Последний вход</Th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <ClickableRow
                  key={u.id}
                  onClick={() => setSelectedId(u.id)}
                  className={selectedId === u.id ? 'bg-surface-strong' : ''}
                >
                  <Td className="font-medium">{u.login}</Td>
                  <Td className="text-fg-secondary">{u.email || '—'}</Td>
                  <Td className="text-fg-secondary">{u.role}</Td>
                  <Td>
                    <div className="flex flex-wrap gap-1">
                      {u.isBanned && <Badge tone="danger">Бан</Badge>}
                      {u.isHwidBanned && <Badge tone="danger">HWID</Badge>}
                      {u.totpEnabled && <Badge tone="ok">2FA</Badge>}
                      {u.telegramId ? <Badge>TG</Badge> : null}
                    </div>
                  </Td>
                  <Td className="whitespace-nowrap text-fg-secondary">{fmtDate(u.lastLoginAt)}</Td>
                </ClickableRow>
              ))}
            </tbody>
          </Table>
        )}

        {totalPages > 1 && (
          <div className="flex items-center justify-center gap-3">
            <IconButton aria-label="Назад" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
              <ChevronLeft size={16} />
            </IconButton>
            <span className="text-sm text-fg-muted">
              {page} / {totalPages}
            </span>
            <IconButton aria-label="Вперёд" disabled={page >= totalPages} onClick={() => setPage((p) => p + 1)}>
              <ChevronRight size={16} />
            </IconButton>
          </div>
        )}
      </div>

      {selectedId && (
        <UserDetailPanel
          userId={selectedId}
          onClose={() => setSelectedId(null)}
          onChanged={loadUsers}
          onDeleted={() => {
            setSelectedId(null);
            void loadUsers();
          }}
        />
      )}
    </div>
  );
}
