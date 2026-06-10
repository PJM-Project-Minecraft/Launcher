'use client';

import { useCallback, useEffect, useState } from 'react';
import { motion } from 'framer-motion';
import { X } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { AuditLogEntry, AuthLogEntry, UserDetail } from '../../app/lib/types';
import { Badge } from '../ui/badge';
import { Button, IconButton } from '../ui/button';
import { Card } from '../ui/card';
import { useConfirm } from '../ui/confirm';
import { Field } from '../ui/field';
import { Select } from '../ui/input';
import { Skeleton } from '../ui/skeleton';
import { useToast } from '../ui/toast';

const ROLES = ['user', 'moderator', 'admin'];

function fmtDate(value?: string | null) {
  return value ? new Date(value).toLocaleString('ru-RU') : '—';
}

function InfoRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3 py-1 text-sm">
      <span className="shrink-0 text-fg-muted">{label}</span>
      <span className="break-all text-right text-fg">{value}</span>
    </div>
  );
}

/** Панель деталей пользователя: данные, роль, бан-действия, журналы. */
export function UserDetailPanel({
  userId,
  onClose,
  onChanged,
  onDeleted
}: {
  userId: string;
  onClose: () => void;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const [detail, setDetail] = useState<UserDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);

  const loadDetail = useCallback(async () => {
    try {
      setDetail(await api<UserDetail>(`/api/admin/users/${userId}`));
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [userId, toast]);

  useEffect(() => {
    setLoading(true);
    void loadDetail();
  }, [loadDetail]);

  /** Общий сценарий мутации: api → toast → refetch деталей и списка. */
  async function mutate(action: () => Promise<unknown>, successText: string) {
    setBusy(true);
    try {
      await action();
      toast('success', successText);
      await loadDetail();
      onChanged();
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function changeRole(role: string) {
    await mutate(
      () => api(`/api/admin/users/${userId}/role`, { method: 'PATCH', body: { role } }),
      `Роль изменена на ${role}`
    );
  }

  async function toggleBan() {
    if (!detail) return;
    const { login, isBanned } = detail.user;
    if (!isBanned) {
      const ok = await confirm({
        title: 'Бан пользователя',
        message: `Забанить пользователя ${login}?`,
        confirmLabel: 'Забанить',
        danger: true
      });
      if (!ok) return;
    }
    await mutate(
      () => api(`/api/admin/users/${userId}/${isBanned ? 'unban' : 'ban'}`, { method: 'POST' }),
      isBanned ? 'Пользователь разбанен' : 'Пользователь забанен'
    );
  }

  async function toggleHwidBan() {
    if (!detail) return;
    const { login, isHwidBanned } = detail.user;
    const ok = await confirm({
      title: isHwidBanned ? 'Снятие HWID-бана' : 'HWID-бан',
      message: isHwidBanned
        ? `Снять HWID-бан с пользователя ${login}?`
        : `Выдать HWID-бан пользователю ${login}?`,
      confirmLabel: isHwidBanned ? 'Снять' : 'Забанить',
      danger: !isHwidBanned
    });
    if (!ok) return;
    await mutate(
      () => api(`/api/admin/users/${userId}/${isHwidBanned ? 'hwid-unban' : 'hwid-ban'}`, { method: 'POST' }),
      isHwidBanned ? 'HWID-бан снят' : 'HWID-бан выдан'
    );
  }

  async function deleteUser() {
    if (!detail) return;
    const ok = await confirm({
      title: 'Удаление пользователя',
      message: `Удалить пользователя ${detail.user.login}? Действие необратимо.`,
      confirmLabel: 'Удалить',
      danger: true
    });
    if (!ok) return;
    setBusy(true);
    try {
      await api(`/api/admin/users/${userId}`, { method: 'DELETE' });
      toast('success', `Пользователь ${detail.user.login} удалён`);
      onDeleted();
    } catch (e) {
      toast('error', errorMessage(e));
      setBusy(false);
    }
  }

  return (
    <motion.div initial={{ opacity: 0, x: 16 }} animate={{ opacity: 1, x: 0 }} transition={{ duration: 0.25 }}>
      <Card className="flex flex-col gap-5">
        {loading || !detail ? (
          <div className="flex flex-col gap-3">
            <Skeleton className="h-6 w-32" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-2/3" />
          </div>
        ) : (
          <>
            <div className="flex items-center justify-between gap-3">
              <div className="flex min-w-0 items-center gap-2">
                <h2 className="truncate text-lg font-bold">{detail.user.login}</h2>
                {detail.user.isBanned && <Badge tone="danger">Бан</Badge>}
                {detail.user.isHwidBanned && <Badge tone="danger">HWID</Badge>}
              </div>
              <IconButton aria-label="Закрыть" onClick={onClose}>
                <X size={16} />
              </IconButton>
            </div>

            <div>
              <InfoRow label="Логин" value={detail.user.login} />
              <InfoRow label="Email" value={detail.user.email || '—'} />
              <InfoRow label="UUID" value={detail.user.providerUuid} />
              <InfoRow
                label="Telegram"
                value={
                  detail.user.telegramId
                    ? `${detail.user.telegramId}${detail.user.telegramUsername ? ` @${detail.user.telegramUsername}` : ''}`
                    : '—'
                }
              />
              <InfoRow label="Создан" value={fmtDate(detail.user.createdAt)} />
              <InfoRow label="Последний вход" value={fmtDate(detail.user.lastLoginAt)} />
            </div>

            <Field label="Роль">
              <Select value={detail.user.role} disabled={busy} onChange={(e) => void changeRole(e.target.value)}>
                {ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </Select>
            </Field>

            <div className="flex flex-wrap gap-2">
              <Button variant={detail.user.isBanned ? 'ghost' : 'danger'} loading={busy} onClick={() => void toggleBan()}>
                {detail.user.isBanned ? 'Разбанить' : 'Забанить'}
              </Button>
              <Button variant="ghost" loading={busy} onClick={() => void toggleHwidBan()}>
                {detail.user.isHwidBanned ? 'Снять HWID-бан' : 'HWID-бан'}
              </Button>
              <Button variant="danger" loading={busy} onClick={() => void deleteUser()}>
                Удалить
              </Button>
            </div>

            <div>
              <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">Журнал входов</h3>
              <AuthLogList logs={detail.authLogs ?? []} />
            </div>

            <div>
              <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">
                Действия администраторов
              </h3>
              <AuditLogList logs={detail.auditLogs ?? []} />
            </div>
          </>
        )}
      </Card>
    </motion.div>
  );
}

function AuthLogList({ logs }: { logs: AuthLogEntry[] }) {
  if (logs.length === 0) return <p className="text-xs text-fg-faint">Пусто</p>;
  return (
    <ul className="flex max-h-56 flex-col gap-2 overflow-y-auto pr-1">
      {logs.map((log) => (
        <li key={log.id} className="flex items-baseline gap-2 text-xs">
          <span
            className={`mt-px size-1.5 shrink-0 self-center rounded-full ${log.success ? 'bg-ok' : 'bg-danger'}`}
            title={log.success ? 'Успех' : 'Ошибка'}
          />
          <span className="min-w-0 break-all text-fg-secondary">
            {log.ip} · {log.source}
            {log.message ? ` · ${log.message}` : ''}
          </span>
          <span className="ml-auto shrink-0 whitespace-nowrap text-fg-faint">{fmtDate(log.createdAt)}</span>
        </li>
      ))}
    </ul>
  );
}

function AuditLogList({ logs }: { logs: AuditLogEntry[] }) {
  if (logs.length === 0) return <p className="text-xs text-fg-faint">Пусто</p>;
  return (
    <ul className="flex max-h-56 flex-col gap-2 overflow-y-auto pr-1">
      {logs.map((log) => (
        <li key={log.id} className="flex items-baseline gap-2 text-xs">
          <span className="min-w-0 break-all text-fg-secondary">
            {log.action}
            {log.details ? ` · ${log.details}` : ''}
          </span>
          <span className="ml-auto shrink-0 whitespace-nowrap text-fg-faint">{fmtDate(log.createdAt)}</span>
        </li>
      ))}
    </ul>
  );
}
