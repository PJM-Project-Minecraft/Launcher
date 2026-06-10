// Чистые хелперы страницы профилей: дефолты формы, нормализация, слаг, форматирование.

import type { LoaderCatalog, Profile, ProfileForm } from '../../app/lib/types';

export const defaultPreservePaths = [
  'saves/',
  'resourcepacks/',
  'shaderpacks/',
  'screenshots/',
  'logs/',
  'crash-reports/',
  'options.txt',
  'optionsof.txt',
  'servers.dat'
];

export const emptyProfile: ProfileForm = {
  name: '',
  slug: '',
  description: '',
  loader: 'vanilla',
  gameVersion: '1.21.1',
  loaderVersion: '',
  javaVersion: 21,
  jvmArgs: '',
  iconUrl: '',
  javaPathWindows: 'runtime/windows-x64/bin/java.exe',
  javaPathLinux: 'runtime/linux/bin/java',
  javaPathMacos: 'runtime/mac-os/jre.bundle/Contents/Home/bin/java',
  launchCommandWindows: '{java} {jvm_args} -jar client.jar --username {login} --uuid {uuid} --accessToken {access_token} --gameDir {game_dir}',
  launchCommandLinux: '{java} {jvm_args} -jar client.jar --username {login} --uuid {uuid} --accessToken {access_token} --gameDir {game_dir}',
  launchCommandMacos: '{java} {jvm_args} -jar client.jar --username {login} --uuid {uuid} --accessToken {access_token} --gameDir {game_dir}',
  preservePaths: [...defaultPreservePaths],
  isActive: true
};

export function profileToForm(profile: Profile): ProfileForm {
  return {
    name: profile.name,
    slug: profile.slug,
    description: profile.description ?? '',
    loader: profile.loader,
    gameVersion: profile.gameVersion,
    loaderVersion: profile.loaderVersion ?? '',
    javaVersion: profile.javaVersion,
    jvmArgs: profile.jvmArgs ?? '',
    iconUrl: profile.iconUrl ?? '',
    javaPathWindows: profile.javaPathWindows ?? '',
    javaPathLinux: profile.javaPathLinux ?? '',
    javaPathMacos: profile.javaPathMacos ?? '',
    launchCommandWindows: profile.launchCommandWindows ?? '',
    launchCommandLinux: profile.launchCommandLinux ?? '',
    launchCommandMacos: profile.launchCommandMacos ?? '',
    preservePaths: profile.preservePaths?.length ? profile.preservePaths : [...defaultPreservePaths],
    isActive: profile.isActive
  };
}

export function normalizeProfileBeforeSave(profile: ProfileForm): ProfileForm {
  return {
    ...profile,
    slug: profile.slug || folderNameFromTitle(profile.name),
    preservePaths: normalizePreservePathsForSave(profile.preservePaths)
  };
}

export function preservePathsToText(paths: string[]) {
  return paths.join('\n');
}

export function textToPreservePaths(value: string) {
  return value.split(/\r?\n/);
}

export function normalizePreservePathsForSave(paths: string[]) {
  const result: string[] = [];
  const seen = new Set<string>();
  for (const item of paths) {
    const normalized = normalizePreservePath(item);
    if (!normalized || seen.has(normalized)) {
      continue;
    }
    seen.add(normalized);
    result.push(normalized);
  }
  return result.length ? result : [...defaultPreservePaths];
}

function normalizePreservePath(value: string) {
  const normalized = value.trim().replace(/\\/g, '/').replace(/\/+/g, '/');
  if (!normalized) {
    return '';
  }
  const isDirectory = normalized.endsWith('/');
  const withoutTrailingSlash = normalized.replace(/\/+$/g, '');
  return `${withoutTrailingSlash}${isDirectory ? '/' : ''}`;
}

export function folderNameFromTitle(value: string) {
  const normalized = value
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9а-яё_-]+/gi, '-')
    .replace(/-+/g, '-')
    .replace(/^-|-$/g, '');
  return transliterate(normalized) || 'profile';
}

function transliterate(value: string) {
  const map: Record<string, string> = {
    а: 'a', б: 'b', в: 'v', г: 'g', д: 'd', е: 'e', ё: 'e', ж: 'zh', з: 'z',
    и: 'i', й: 'y', к: 'k', л: 'l', м: 'm', н: 'n', о: 'o', п: 'p', р: 'r',
    с: 's', т: 't', у: 'u', ф: 'f', х: 'h', ц: 'c', ч: 'ch', ш: 'sh', щ: 'sch',
    ъ: '', ы: 'y', ь: '', э: 'e', ю: 'yu', я: 'ya'
  };
  return value
    .split('')
    .map((char) => map[char] ?? char)
    .join('')
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/-+/g, '-')
    .replace(/^-|-$/g, '');
}

export function loaderLabel(loaderId: string, catalog: LoaderCatalog) {
  return catalog.loaders.find((loader) => loader.id === loaderId)?.label ?? loaderId;
}

export function javaVersionForMinecraft(gameVersion: string) {
  if (gameVersion.startsWith('1.21') || gameVersion.startsWith('1.20.5') || gameVersion.startsWith('1.20.6')) {
    return 21;
  }
  if (gameVersion.startsWith('1.18') || gameVersion.startsWith('1.19') || gameVersion.startsWith('1.20')) {
    return 17;
  }
  if (gameVersion.startsWith('1.17')) {
    return 16;
  }
  return 8;
}

export function formatBytes(value: number) {
  if (!Number.isFinite(value) || value <= 0) {
    return '0 B';
  }
  const units = ['B', 'KB', 'MB', 'GB'];
  let amount = value;
  let unitIndex = 0;
  while (amount >= 1024 && unitIndex < units.length - 1) {
    amount /= 1024;
    unitIndex++;
  }
  return `${amount.toFixed(unitIndex === 0 ? 0 : 1)} ${units[unitIndex]}`;
}

export function formatDate(value: string) {
  return new Date(value).toLocaleString('ru-RU');
}
