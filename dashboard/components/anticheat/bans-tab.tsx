'use client';

import { FormEvent, useState } from 'react';
import { UserCheck, MonitorOff } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { AccountBan, HwidBan } from '../../app/lib/types';
import { Card } from '../ui/card';
import { Table, Th, Td } from '../ui/table';
import { Button } from '../ui/button';
import { Input } from '../ui/input';
import { Field } from '../ui/field';
import { SkeletonTable } from '../ui/skeleton';
import { EmptyState } from '../ui/empty-state';
import { useToast } from '../ui/toast';
import { useConfirm } from '../ui/confirm';

export function BansTab({
  accountBans,
  hwidBans,
  loading,
  onReload
}: {
  accountBans: AccountBan[];
  hwidBans: HwidBan[];
  loading: boolean;
  onReload: () => Promise<void>;
}) {
  const toast = useToast();
  const confirm = useConfirm();

  const [accountForm, setAccountForm] = useState({ userUuid: '', login: '', reason: '' });
  const [hwidForm, setHwidForm] = useState({ hwidHash: '', reason: '' });
  const [submittingAccount, setSubmittingAccount] = useState(false);
  const [submittingHwid, setSubmittingHwid] = useState(false);

  async function unbanAccount(ban: AccountBan) {
    const ok = await confirm({
      title: 'Снять бан аккаунта',
      message: `Снять бан с аккаунта ${ban.login || ban.userUuid}?`,
      confirmLabel: 'Разбанить'
    });
    if (!ok) return;
    try {
      await api(`/api/admin/anticheat/bans/account/${encodeURIComponent(ban.userUuid)}`, { method: 'DELETE' });
      toast('success', `Бан аккаунта ${ban.login || ban.userUuid} снят`);
      await onReload();
    } catch (e) {
      toast('error', errorMessage(e));
    }
  }

  async function unbanHwid(ban: HwidBan) {
    const ok = await confirm({
      title: 'Снять HWID-бан',
      message: `Снять бан устройства ${ban.hwidHash.slice(0, 24)}…?`,
      confirmLabel: 'Разбанить'
    });
    if (!ok) return;
    try {
      await api(`/api/admin/anticheat/bans/hwid/${encodeURIComponent(ban.hwidHash)}`, { method: 'DELETE' });
      toast('success', 'HWID-бан снят');
      await onReload();
    } catch (e) {
      toast('error', errorMessage(e));
    }
  }

  async function banAccount(e: FormEvent) {
    e.preventDefault();
    setSubmittingAccount(true);
    try {
      await api('/api/admin/anticheat/bans/account', { method: 'POST', body: accountForm });
      toast('success', `Аккаунт ${accountForm.login || accountForm.userUuid} забанен`);
      setAccountForm({ userUuid: '', login: '', reason: '' });
      await onReload();
    } catch (err) {
      toast('error', errorMessage(err));
    } finally {
      setSubmittingAccount(false);
    }
  }

  async function banHwid(e: FormEvent) {
    e.preventDefault();
    setSubmittingHwid(true);
    try {
      await api('/api/admin/anticheat/bans/hwid', { method: 'POST', body: hwidForm });
      toast('success', 'HWID-бан добавлен');
      setHwidForm({ hwidHash: '', reason: '' });
      await onReload();
    } catch (err) {
      toast('error', errorMessage(err));
    } finally {
      setSubmittingHwid(false);
    }
  }

  if (loading) {
    return (
      <div className="flex flex-col gap-4">
        <SkeletonTable rows={3} cols={5} />
        <SkeletonTable rows={3} cols={4} />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-secondary">Баны аккаунтов</h2>
        {accountBans.length === 0 ? (
          <EmptyState icon={UserCheck} title="Нет банов аккаунтов" />
        ) : (
          <Table className="mb-4">
            <thead>
              <tr>
                <Th>Игрок</Th>
                <Th>UUID</Th>
                <Th>Причина</Th>
                <Th>Кем</Th>
                <Th>Дата</Th>
                <Th />
              </tr>
            </thead>
            <tbody>
              {accountBans.map((b) => (
                <tr key={b.id}>
                  <Td className="font-medium">{b.login || '—'}</Td>
                  <Td className="font-mono text-xs text-fg-muted" title={b.userUuid}>
                    {b.userUuid.slice(0, 12)}…
                  </Td>
                  <Td className="text-fg-secondary">{b.reason || 'без причины'}</Td>
                  <Td className="text-fg-muted">{b.bannedBy}</Td>
                  <Td className="whitespace-nowrap text-fg-muted">{new Date(b.createdAt).toLocaleString('ru-RU')}</Td>
                  <Td className="text-right">
                    <Button className="h-8 px-3" onClick={() => void unbanAccount(b)}>
                      Разбанить
                    </Button>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        <form onSubmit={banAccount} className="mt-4 grid items-end gap-3 sm:grid-cols-[1fr_1fr_1fr_auto]">
          <Field label="UUID игрока">
            <Input
              required
              value={accountForm.userUuid}
              onChange={(e) => setAccountForm({ ...accountForm, userUuid: e.target.value })}
              placeholder="uuid"
            />
          </Field>
          <Field label="Логин">
            <Input
              value={accountForm.login}
              onChange={(e) => setAccountForm({ ...accountForm, login: e.target.value })}
              placeholder="login"
            />
          </Field>
          <Field label="Причина">
            <Input
              value={accountForm.reason}
              onChange={(e) => setAccountForm({ ...accountForm, reason: e.target.value })}
              placeholder="причина бана"
            />
          </Field>
          <Button type="submit" variant="danger" loading={submittingAccount}>
            Забанить
          </Button>
        </form>
      </Card>

      <Card>
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-secondary">HWID-баны</h2>
        {hwidBans.length === 0 ? (
          <EmptyState icon={MonitorOff} title="Нет HWID-банов" />
        ) : (
          <Table className="mb-4">
            <thead>
              <tr>
                <Th>HWID</Th>
                <Th>Причина</Th>
                <Th>Кем</Th>
                <Th>Дата</Th>
                <Th />
              </tr>
            </thead>
            <tbody>
              {hwidBans.map((b) => (
                <tr key={b.id}>
                  <Td className="font-mono text-xs" title={b.hwidHash}>
                    {b.hwidHash.slice(0, 24)}…
                  </Td>
                  <Td className="text-fg-secondary">{b.reason || 'без причины'}</Td>
                  <Td className="text-fg-muted">{b.bannedBy}</Td>
                  <Td className="whitespace-nowrap text-fg-muted">{new Date(b.createdAt).toLocaleString('ru-RU')}</Td>
                  <Td className="text-right">
                    <Button className="h-8 px-3" onClick={() => void unbanHwid(b)}>
                      Разбанить
                    </Button>
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
        <form onSubmit={banHwid} className="mt-4 grid items-end gap-3 sm:grid-cols-[2fr_1fr_auto]">
          <Field label="HWID-хеш">
            <Input
              required
              value={hwidForm.hwidHash}
              onChange={(e) => setHwidForm({ ...hwidForm, hwidHash: e.target.value })}
              placeholder="sha-256 hex"
            />
          </Field>
          <Field label="Причина">
            <Input
              value={hwidForm.reason}
              onChange={(e) => setHwidForm({ ...hwidForm, reason: e.target.value })}
              placeholder="причина бана"
            />
          </Field>
          <Button type="submit" variant="danger" loading={submittingHwid}>
            Забанить
          </Button>
        </form>
      </Card>
    </div>
  );
}
