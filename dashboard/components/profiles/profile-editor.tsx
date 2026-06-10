'use client';

// Правая колонка: редактор профиля с табами секций, сохранением и удалением.

import { useEffect, useMemo, useState } from 'react';
import { Trash2 } from 'lucide-react';
import { Badge } from '../ui/badge';
import { Button } from '../ui/button';
import { Card } from '../ui/card';
import { Tabs } from '../ui/tabs';
import { useConfirm } from '../ui/confirm';
import { useToast } from '../ui/toast';
import { api, errorMessage } from '../../app/lib/api';
import type { LoaderCatalog, Profile, ProfileForm } from '../../app/lib/types';
import {
  emptyProfile,
  defaultPreservePaths,
  folderNameFromTitle,
  javaVersionForMinecraft,
  normalizeProfileBeforeSave,
  profileToForm
} from './helpers';
import { JavaSection, LaunchSection, LoaderSection, MainSection, SecuritySection } from './profile-editor-sections';
import { ClientFlow } from './client-flow';

type EditorSection = 'main' | 'loader' | 'java' | 'launch' | 'security' | 'build';

const sectionTabs: Array<{ key: EditorSection; label: string }> = [
  { key: 'main', label: 'Основное' },
  { key: 'loader', label: 'Загрузчик' },
  { key: 'java', label: 'Java' },
  { key: 'launch', label: 'Запуск' },
  { key: 'security', label: 'Защита' },
  { key: 'build', label: 'Сборка' }
];

export function ProfileEditor({
  profile,
  onSaved,
  onDeleted,
  onRefresh
}: {
  profile: Profile | null;
  onSaved: (profile: Profile) => void;
  onDeleted: () => void;
  onRefresh: () => Promise<void>;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const [form, setForm] = useState<ProfileForm>(() =>
    profile ? profileToForm(profile) : { ...emptyProfile, preservePaths: [...defaultPreservePaths] }
  );
  const [folderEdited, setFolderEdited] = useState(Boolean(profile));
  const [section, setSection] = useState<EditorSection>('main');
  const [isSaving, setIsSaving] = useState(false);
  const [loaderCatalog, setLoaderCatalog] = useState<LoaderCatalog>({ minecraftVersions: [], loaders: [] });
  const [isLoadingLoaders, setIsLoadingLoaders] = useState(false);

  const selectedLoader = useMemo(
    () => loaderCatalog.loaders.find((loader) => loader.id === form.loader),
    [loaderCatalog.loaders, form.loader]
  );

  // Каталог лоадеров подгружается по версии игры с дебаунсом.
  useEffect(() => {
    const timeout = window.setTimeout(() => {
      void (async () => {
        setIsLoadingLoaders(true);
        try {
          const data = await api<LoaderCatalog>(
            `/api/admin/profiles/loader-options?gameVersion=${encodeURIComponent(form.gameVersion)}`
          );
          setLoaderCatalog(data);
        } catch {
          setLoaderCatalog({ minecraftVersions: [], loaders: [] });
        } finally {
          setIsLoadingLoaders(false);
        }
      })();
    }, 250);
    return () => window.clearTimeout(timeout);
  }, [form.gameVersion]);

  // Подгонка формы под доступные лоадеры и их версии.
  useEffect(() => {
    if (loaderCatalog.loaders.length === 0) {
      return;
    }
    setForm((current) => {
      const loader = loaderCatalog.loaders.find((item) => item.id === current.loader);
      if (!loader) {
        const fallback = loaderCatalog.loaders.find((item) => item.id === 'vanilla') ?? loaderCatalog.loaders[0];
        return {
          ...current,
          loader: fallback.id,
          javaVersion: fallback.javaVersion,
          loaderVersion: fallback.requiresVersion ? fallback.versions[0]?.value ?? '' : ''
        };
      }
      if (!loader.requiresVersion) {
        return current.loaderVersion ? { ...current, loaderVersion: '' } : current;
      }
      const latestVersion = loader.versions[0]?.value ?? '';
      if (!latestVersion || loader.versions.some((version) => version.value === current.loaderVersion)) {
        return current;
      }
      return {
        ...current,
        javaVersion: loader.javaVersion,
        loaderVersion: latestVersion
      };
    });
  }, [loaderCatalog.loaders]);

  function updateName(name: string) {
    setForm((current) => ({
      ...current,
      name,
      slug: folderEdited ? current.slug : folderNameFromTitle(name)
    }));
  }

  function updateLoader(loaderId: string) {
    const loader = loaderCatalog.loaders.find((item) => item.id === loaderId);
    setForm((current) => ({
      ...current,
      loader: loaderId,
      javaVersion: loader?.javaVersion ?? current.javaVersion,
      loaderVersion: loader?.requiresVersion ? loader.versions[0]?.value ?? current.loaderVersion : ''
    }));
  }

  function updateGameVersion(gameVersion: string) {
    setForm((current) => ({
      ...current,
      gameVersion,
      javaVersion: javaVersionForMinecraft(gameVersion),
      loaderVersion: ''
    }));
  }

  async function save() {
    setIsSaving(true);
    try {
      const payload = normalizeProfileBeforeSave(form);
      const saved = profile
        ? await api<Profile>(`/api/admin/profiles/${profile.id}`, { method: 'PATCH', body: payload })
        : await api<Profile>('/api/admin/profiles', { method: 'POST', body: payload });
      toast('success', `Профиль сохранён: ${saved.name}`);
      onSaved(saved);
    } catch (error) {
      toast('error', errorMessage(error));
    } finally {
      setIsSaving(false);
    }
  }

  async function remove() {
    if (!profile) return;
    const ok = await confirm({
      title: 'Удалить профиль',
      message: `Профиль «${profile.name}» будет удалён из БД. Файлы на диске останутся.`,
      confirmLabel: 'Удалить',
      danger: true
    });
    if (!ok) return;
    try {
      await api(`/api/admin/profiles/${profile.id}`, { method: 'DELETE' });
      toast('success', `Профиль удалён: ${profile.name}`);
      onDeleted();
    } catch (error) {
      toast('error', errorMessage(error));
    }
  }

  return (
    <Card className="flex flex-col gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <div className="text-xs uppercase tracking-wide text-fg-muted">{profile ? 'Редактирование' : 'Создание'}</div>
          <h2 className="text-lg font-semibold text-fg">{profile?.name || form.name || 'Новый профиль'}</h2>
        </div>
        <Badge>manifest v{profile?.manifestVersion ?? 0}</Badge>
      </div>

      <div className="overflow-x-auto">
        <Tabs items={sectionTabs} active={section} onChange={(key) => setSection(key as EditorSection)} />
      </div>

      <form
        className="flex flex-col gap-5"
        onSubmit={(event) => {
          event.preventDefault();
          void save();
        }}
      >
        {section === 'main' && (
          <MainSection form={form} setForm={setForm} onNameChange={updateName} onSlugEdited={() => setFolderEdited(true)} />
        )}
        {section === 'loader' && (
          <LoaderSection
            form={form}
            setForm={setForm}
            catalog={loaderCatalog}
            selectedLoader={selectedLoader}
            isLoadingLoaders={isLoadingLoaders}
            onGameVersionChange={updateGameVersion}
            onLoaderChange={updateLoader}
          />
        )}
        {section === 'java' && <JavaSection form={form} setForm={setForm} />}
        {section === 'launch' && <LaunchSection form={form} setForm={setForm} />}
        {section === 'security' && <SecuritySection form={form} setForm={setForm} />}
        {section === 'build' && <ClientFlow profile={profile} onRefresh={onRefresh} />}

        <div className="flex flex-wrap items-center justify-between gap-2 border-t border-edge pt-4">
          <Button type="submit" variant="primary" loading={isSaving}>
            {profile ? 'Сохранить профиль' : 'Создать профиль'}
          </Button>
          {profile && (
            <Button type="button" variant="danger" onClick={() => void remove()}>
              <Trash2 size={15} />
              Удалить
            </Button>
          )}
        </div>
      </form>
    </Card>
  );
}
