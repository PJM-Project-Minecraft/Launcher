'use client';

import { FormEvent, useState } from 'react';
import { FileSearch } from 'lucide-react';
import { api, errorMessage } from '../../app/lib/api';
import type { CheatSignature } from '../../app/lib/types';
import { Card } from '../ui/card';
import { Table, Th, Td } from '../ui/table';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { Input, Select } from '../ui/input';
import { Field } from '../ui/field';
import { SkeletonTable } from '../ui/skeleton';
import { EmptyState } from '../ui/empty-state';
import { useToast } from '../ui/toast';
import { useConfirm } from '../ui/confirm';

function severityTone(severity: number): 'danger' | 'warn' | 'default' {
  if (severity >= 8) return 'danger';
  if (severity >= 5) return 'warn';
  return 'default';
}

const emptyForm = { kind: 'process', pattern: '', hashHex: '', severity: 5, note: '' };

export function SignaturesTab({
  signatures,
  loading,
  onReload
}: {
  signatures: CheatSignature[];
  loading: boolean;
  onReload: () => Promise<void>;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const [form, setForm] = useState(emptyForm);
  const [submitting, setSubmitting] = useState(false);

  async function createSignature(e: FormEvent) {
    e.preventDefault();
    setSubmitting(true);
    try {
      await api('/api/admin/anticheat/signatures', {
        method: 'POST',
        body: { ...form, severity: Number(form.severity), enabled: true }
      });
      toast('success', 'Сигнатура добавлена');
      setForm(emptyForm);
      await onReload();
    } catch (err) {
      toast('error', errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  }

  async function deleteSignature(sig: CheatSignature) {
    const ok = await confirm({
      title: 'Удалить сигнатуру',
      message: `Удалить сигнатуру «${sig.pattern || sig.hashHex.slice(0, 16)}» (${sig.kind})?`,
      confirmLabel: 'Удалить',
      danger: true
    });
    if (!ok) return;
    try {
      await api(`/api/admin/anticheat/signatures/${sig.id}`, { method: 'DELETE' });
      toast('success', 'Сигнатура удалена');
      await onReload();
    } catch (e) {
      toast('error', errorMessage(e));
    }
  }

  if (loading) {
    return <SkeletonTable rows={5} cols={6} />;
  }

  return (
    <div className="flex flex-col gap-4">
      {signatures.length === 0 ? (
        <EmptyState icon={FileSearch} title="Блэклист пуст" hint="Добавьте первую сигнатуру через форму ниже." />
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Тип</Th>
              <Th>Паттерн / хеш</Th>
              <Th>Severity</Th>
              <Th>Заметка</Th>
              <Th>Статус</Th>
              <Th />
            </tr>
          </thead>
          <tbody>
            {signatures.map((s) => (
              <tr key={s.id}>
                <Td className="font-medium">{s.kind}</Td>
                <Td className="font-mono text-xs text-fg-secondary" title={s.pattern || s.hashHex}>
                  {s.pattern || `${s.hashHex.slice(0, 16)}…`}
                </Td>
                <Td>
                  <Badge tone={severityTone(s.severity)}>{s.severity}</Badge>
                </Td>
                <Td className="text-fg-muted">{s.note || '—'}</Td>
                <Td>
                  <Badge tone={s.enabled ? 'ok' : 'default'}>{s.enabled ? 'Включена' : 'Выключена'}</Badge>
                </Td>
                <Td className="text-right">
                  <Button variant="danger" className="h-8 px-3" onClick={() => void deleteSignature(s)}>
                    Удалить
                  </Button>
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}

      <Card>
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-secondary">Новая сигнатура</h2>
        <form onSubmit={createSignature} className="grid items-end gap-3 sm:grid-cols-2 lg:grid-cols-[auto_1fr_1fr_auto_1fr_auto]">
          <Field label="Тип">
            <Select value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
              <option value="process">process</option>
              <option value="class">class</option>
              <option value="jar">jar</option>
              <option value="file">file</option>
            </Select>
          </Field>
          <Field label="Паттерн (имя/подстрока)">
            <Input
              value={form.pattern}
              onChange={(e) => setForm({ ...form, pattern: e.target.value })}
              placeholder="cheatengine"
            />
          </Field>
          <Field label="SHA-256 (опц.)">
            <Input
              value={form.hashHex}
              onChange={(e) => setForm({ ...form, hashHex: e.target.value })}
              placeholder="hex"
            />
          </Field>
          <Field label="Severity">
            <Input
              type="number"
              min={1}
              max={10}
              className="w-20"
              value={form.severity}
              onChange={(e) => setForm({ ...form, severity: Number(e.target.value) })}
            />
          </Field>
          <Field label="Заметка">
            <Input
              value={form.note}
              onChange={(e) => setForm({ ...form, note: e.target.value })}
              placeholder="комментарий"
            />
          </Field>
          <Button type="submit" variant="primary" loading={submitting}>
            Добавить
          </Button>
        </form>
      </Card>
    </div>
  );
}
