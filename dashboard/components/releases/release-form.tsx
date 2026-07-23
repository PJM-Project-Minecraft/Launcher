'use client';

// Форма нового релиза лаунчера: версия, changelog, флаг «обязательный»,
// бинарники по платформам. Загрузка multipart с прогрессом (apiUpload).

import { useState } from 'react';
import { Button } from '../ui/button';
import { Card } from '../ui/card';
import { Field } from '../ui/field';
import { Input, TextArea } from '../ui/input';
import { useToast } from '../ui/toast';
import { apiUpload, errorMessage } from '../../app/lib/api';

const platforms = [
  { key: 'linux-x64', label: 'Linux x64' },
  { key: 'windows-x64', label: 'Windows x64' }
] as const;

export function ReleaseForm({ onCreated }: { onCreated: () => void }) {
  const toast = useToast();
  const [formKey, setFormKey] = useState(0);
  const [version, setVersion] = useState('');
  const [changelog, setChangelog] = useState('');
  const [mandatory, setMandatory] = useState(false);
  const [files, setFiles] = useState<Record<string, File | null>>({
    'linux-x64': null,
    'windows-x64': null
  });
  // Оффлайн Ed25519-подпись бинарника (hex из `updatesign sign`). Необязательна, но
  // лаунчер со вшитым ключом примет обновление ТОЛЬКО с валидной подписью.
  const [signatures, setSignatures] = useState<Record<string, string>>({
    'linux-x64': '',
    'windows-x64': ''
  });
  const [progress, setProgress] = useState<number | null>(null);

  const selectedCount = Object.values(files).filter(Boolean).length;
  const canSubmit = /^\d+\.\d+\.\d+$/.test(version.trim()) && selectedCount > 0 && progress === null;

  async function submit() {
    const form = new FormData();
    form.set('version', version.trim());
    form.set('changelog', changelog.trim());
    form.set('mandatory', mandatory ? 'true' : 'false');
    for (const platform of platforms) {
      const file = files[platform.key];
      if (file) form.set(platform.key, file);
      const sig = signatures[platform.key]?.trim();
      if (sig) form.set(`signature-${platform.key}`, sig);
    }

    setProgress(0);
    try {
      await apiUpload('/api/admin/releases/', form, setProgress);
      toast('success', `Релиз ${version.trim()} опубликован`);
      setVersion('');
      setChangelog('');
      setMandatory(false);
      setFiles({ 'linux-x64': null, 'windows-x64': null });
      setSignatures({ 'linux-x64': '', 'windows-x64': '' });
      setFormKey((k) => k + 1);
      onCreated();
    } catch (error) {
      toast('error', errorMessage(error));
    } finally {
      setProgress(null);
    }
  }

  return (
    <Card className="flex flex-col gap-4">
      <h2 className="text-sm font-bold uppercase tracking-wide text-fg-muted">Новый релиз</h2>

      <Field label="Версия" hint="Формат X.Y.Z, должна совпадать с version в Cargo.toml сборки">
        <Input value={version} onChange={(e) => setVersion(e.target.value)} placeholder="0.2.0" />
      </Field>

      <Field label="Changelog">
        <TextArea
          value={changelog}
          onChange={(e) => setChangelog(e.target.value)}
          placeholder="Что нового в этой версии"
        />
      </Field>

      {platforms.map((platform) => (
        <div key={`${platform.key}-${formKey}`} className="flex flex-col gap-2">
          <Field label={`Бинарник ${platform.label}`}>
            <input
              type="file"
              onChange={(e) => setFiles((current) => ({ ...current, [platform.key]: e.target.files?.[0] ?? null }))}
              className="block w-full text-sm text-fg-secondary file:mr-3 file:rounded-lg file:border file:border-edge file:bg-surface file:px-3 file:py-2 file:text-sm file:font-semibold file:text-fg hover:file:bg-surface-strong"
            />
          </Field>
          <Field label={`Подпись ${platform.label}`} hint="hex из `updatesign sign` (необязательно, но нужна для лаунчера со вшитым ключом)">
            <Input
              value={signatures[platform.key]}
              onChange={(e) => setSignatures((current) => ({ ...current, [platform.key]: e.target.value }))}
              placeholder="128 hex-символов Ed25519"
            />
          </Field>
        </div>
      ))}

      <label className="flex items-center gap-2 text-sm text-fg-secondary">
        <input type="checkbox" checked={mandatory} onChange={(e) => setMandatory(e.target.checked)} />
        Обязательный — старые лаунчеры не смогут запускать игру, пока не обновятся
      </label>

      {progress !== null && (
        <div className="h-2 w-full overflow-hidden rounded-full bg-surface-strong">
          <div className="h-full bg-fg transition-all" style={{ width: `${Math.round(progress * 100)}%` }} />
        </div>
      )}

      <Button variant="primary" disabled={!canSubmit} loading={progress !== null} onClick={() => void submit()}>
        Опубликовать релиз
      </Button>
    </Card>
  );
}
