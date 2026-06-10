// Общие типы данных админки. Источник истины по полям — backend adminapi.

export type AuthUser = {
  id: string;
  login: string;
  providerUuid?: string;
  role: string;
};

export type Stats = {
  totalUsers: number;
  telegramLinked: number;
  bannedUsers: number;
  hwidBanned: number;
  newUsers7d: number;
  authSuccess24h: number;
  authFailure24h: number;
};

export type AdminUser = {
  id: string;
  login: string;
  providerUuid: string;
  email: string;
  role: string;
  isBanned: boolean;
  isHwidBanned: boolean;
  totpEnabled: boolean;
  telegramId?: number | null;
  telegramUsername?: string;
  createdAt: string;
  lastLoginAt?: string | null;
};

export type AuthLogEntry = {
  id: number;
  username: string;
  ip: string;
  source: string;
  success: boolean;
  message: string;
  createdAt: string;
};

export type AuditLogEntry = {
  id: string;
  action: string;
  details: string;
  createdAt: string;
};

export type UserDetail = {
  user: AdminUser;
  authLogs: AuthLogEntry[];
  auditLogs: AuditLogEntry[];
};

export type Profile = {
  id: string;
  name: string;
  slug: string;
  description: string;
  loader: string;
  gameVersion: string;
  loaderVersion: string;
  javaVersion: number;
  jvmArgs: string;
  iconUrl: string;
  javaPathWindows: string;
  javaPathLinux: string;
  javaPathMacos: string;
  launchCommandWindows: string;
  launchCommandLinux: string;
  launchCommandMacos: string;
  preservePaths: string[];
  manifestVersion: number;
  manifestUpdatedAt?: string;
  isActive: boolean;
  fileCount: number;
  totalSize: number;
  clientPrepared: boolean;
  clientStatus: string;
};

export type ProfileForm = Omit<
  Profile,
  'id' | 'manifestVersion' | 'manifestUpdatedAt' | 'fileCount' | 'totalSize' | 'clientPrepared' | 'clientStatus'
>;

export type LoaderCatalog = {
  minecraftVersions: string[];
  loaders: LoaderOption[];
};

export type LoaderOption = {
  id: string;
  label: string;
  javaVersion: number;
  requiresVersion: boolean;
  versions: LoaderVersion[];
};

export type LoaderVersion = {
  value: string;
  label: string;
  stable: boolean;
};

export type Detection = {
  id: string;
  userUuid: string;
  login: string;
  hwidHash: string;
  source: string;
  type: string;
  signature: string;
  severity: number;
  createdAt: string;
};

export type HwidBan = {
  id: string;
  hwidHash: string;
  reason: string;
  bannedBy: string;
  createdAt: string;
};

export type AccountBan = {
  id: string;
  userUuid: string;
  login: string;
  reason: string;
  bannedBy: string;
  createdAt: string;
};

export type CheatSignature = {
  id: string;
  kind: string;
  pattern: string;
  hashHex: string;
  severity: number;
  note: string;
  enabled: boolean;
};
