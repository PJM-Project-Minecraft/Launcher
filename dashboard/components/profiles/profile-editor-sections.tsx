'use client';

// Секции формы редактора профиля: Основное, Загрузчик, Java, Запуск, Защита.

import { Folder, Lock, Package, Shield } from 'lucide-react';
import { Field } from '../ui/field';
import { Input, Select, TextArea } from '../ui/input';
import type { LoaderCatalog, LoaderOption, ProfileForm } from '../../app/lib/types';
import { folderNameFromTitle, loaderLabel, preservePathsToText, textToPreservePaths } from './helpers';

type SetForm = (updater: (current: ProfileForm) => ProfileForm) => void;

export function MainSection({
  form,
  setForm,
  onNameChange,
  onSlugEdited
}: {
  form: ProfileForm;
  setForm: SetForm;
  onNameChange: (name: string) => void;
  onSlugEdited: () => void;
}) {
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-4 max-md:grid-cols-1">
        <Field label="Название профиля">
          <Input value={form.name} onChange={(e) => onNameChange(e.target.value)} placeholder="Project Survival" />
        </Field>
        <Field label="Папка профиля">
          <Input
            value={form.slug}
            onChange={(e) => {
              onSlugEdited();
              const slug = folderNameFromTitle(e.target.value);
              setForm((current) => ({ ...current, slug }));
            }}
            placeholder="project-survival"
          />
        </Field>
      </div>
      <div className="flex items-center gap-2 rounded-lg border border-edge bg-surface px-3 py-2 text-xs text-fg-muted">
        <Folder size={14} className="shrink-0 text-fg-faint" />
        <span className="truncate">storage/profiles/{form.slug || 'имя-профиля'}/files</span>
      </div>
      <Field label="Описание">
        <TextArea
          value={form.description}
          onChange={(e) => setForm((current) => ({ ...current, description: e.target.value }))}
          rows={3}
        />
      </Field>
      <Field label="Иконка профиля" hint="URL картинки для лаунчера">
        <Input value={form.iconUrl} onChange={(e) => setForm((current) => ({ ...current, iconUrl: e.target.value }))} />
      </Field>
      <label className="flex cursor-pointer items-center gap-2.5 text-sm text-fg-secondary">
        <input
          type="checkbox"
          checked={form.isActive}
          onChange={(e) => setForm((current) => ({ ...current, isActive: e.target.checked }))}
          className="h-4 w-4 accent-current"
        />
        <span>Профиль активен для игроков</span>
      </label>
    </div>
  );
}

export function LoaderSection({
  form,
  setForm,
  catalog,
  selectedLoader,
  isLoadingLoaders,
  onGameVersionChange,
  onLoaderChange
}: {
  form: ProfileForm;
  setForm: SetForm;
  catalog: LoaderCatalog;
  selectedLoader: LoaderOption | undefined;
  isLoadingLoaders: boolean;
  onGameVersionChange: (version: string) => void;
  onLoaderChange: (loaderId: string) => void;
}) {
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-4 max-md:grid-cols-1">
        <Field label="Версия Minecraft">
          <Input
            value={form.gameVersion}
            onChange={(e) => onGameVersionChange(e.target.value)}
            list="minecraft-version-options"
          />
          <datalist id="minecraft-version-options">
            {catalog.minecraftVersions.map((version) => (
              <option key={version} value={version} />
            ))}
          </datalist>
        </Field>
        <Field label="Загрузчик">
          <Select value={form.loader} onChange={(e) => onLoaderChange(e.target.value)}>
            {!catalog.loaders.some((loader) => loader.id === form.loader) && (
              <option value={form.loader}>{form.loader} недоступен для этой версии</option>
            )}
            {catalog.loaders.map((loader) => (
              <option key={loader.id} value={loader.id}>
                {loader.label}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Версия загрузчика">
          {selectedLoader?.versions.length ? (
            <Select
              value={form.loaderVersion}
              onChange={(e) => setForm((current) => ({ ...current, loaderVersion: e.target.value }))}
              disabled={!selectedLoader.requiresVersion}
            >
              {selectedLoader.versions.map((version) => (
                <option key={version.value || 'none'} value={version.value}>
                  {version.label}
                </option>
              ))}
            </Select>
          ) : (
            <Input
              value={form.loaderVersion}
              onChange={(e) => setForm((current) => ({ ...current, loaderVersion: e.target.value }))}
              disabled={form.loader === 'vanilla'}
            />
          )}
        </Field>
        <Field label="Java для версии">
          <Input
            type="number"
            min={8}
            value={form.javaVersion}
            onChange={(e) => setForm((current) => ({ ...current, javaVersion: Number(e.target.value) || 17 }))}
          />
        </Field>
      </div>
      <div className="flex items-center gap-2 rounded-lg border border-edge bg-surface px-3 py-2 text-xs text-fg-muted">
        <Package size={14} className="shrink-0 text-fg-faint" />
        <span>
          {isLoadingLoaders
            ? 'Обновляем список версий…'
            : `${loaderLabel(form.loader, catalog)} ${form.loaderVersion || ''}`.trim()}
        </span>
      </div>
    </div>
  );
}

export function JavaSection({ form, setForm }: { form: ProfileForm; setForm: SetForm }) {
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-2 gap-4 max-md:grid-cols-1">
        <Field label="Java Windows">
          <Input
            value={form.javaPathWindows}
            onChange={(e) => setForm((current) => ({ ...current, javaPathWindows: e.target.value }))}
          />
        </Field>
        <Field label="Java Linux">
          <Input
            value={form.javaPathLinux}
            onChange={(e) => setForm((current) => ({ ...current, javaPathLinux: e.target.value }))}
          />
        </Field>
        <Field label="Java macOS">
          <Input
            value={form.javaPathMacos}
            onChange={(e) => setForm((current) => ({ ...current, javaPathMacos: e.target.value }))}
          />
        </Field>
      </div>
      <Field label="JVM args">
        <TextArea
          value={form.jvmArgs}
          onChange={(e) => setForm((current) => ({ ...current, jvmArgs: e.target.value }))}
          rows={3}
        />
      </Field>
    </div>
  );
}

export function LaunchSection({ form, setForm }: { form: ProfileForm; setForm: SetForm }) {
  return (
    <div className="flex flex-col gap-4">
      <Field label="Команда Windows">
        <TextArea
          value={form.launchCommandWindows}
          onChange={(e) => setForm((current) => ({ ...current, launchCommandWindows: e.target.value }))}
          rows={3}
        />
      </Field>
      <Field label="Команда Linux">
        <TextArea
          value={form.launchCommandLinux}
          onChange={(e) => setForm((current) => ({ ...current, launchCommandLinux: e.target.value }))}
          rows={3}
        />
      </Field>
      <Field label="Команда macOS">
        <TextArea
          value={form.launchCommandMacos}
          onChange={(e) => setForm((current) => ({ ...current, launchCommandMacos: e.target.value }))}
          rows={3}
        />
      </Field>
    </div>
  );
}

export function SecuritySection({ form, setForm }: { form: ProfileForm; setForm: SetForm }) {
  return (
    <div className="flex flex-col gap-4">
      <Field label="Whitelist / не трогать" hint="Один путь на строку, относительно папки установки клиента.">
        <TextArea
          value={preservePathsToText(form.preservePaths)}
          onChange={(e) => setForm((current) => ({ ...current, preservePaths: textToPreservePaths(e.target.value) }))}
          rows={8}
        />
      </Field>
      <div className="flex items-center gap-2 rounded-lg border border-edge bg-surface px-3 py-2 text-xs text-fg-muted">
        <Shield size={14} className="shrink-0 text-fg-faint" />
        <span>Папки заканчиваются «/», пути указываются относительно папки установки клиента.</span>
      </div>
      <div className="flex items-center gap-2 rounded-lg border border-edge bg-surface px-3 py-2 text-xs text-fg-muted">
        <Lock size={14} className="shrink-0 text-fg-faint" />
        <span>mods, libraries, versions, assets и runtime всегда защищены manifest.</span>
      </div>
    </div>
  );
}
