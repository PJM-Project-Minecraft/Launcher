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

export type DetectionStatus = 'new' | 'reviewed' | 'confirmed' | 'dismissed';

export type Detection = {
  id: string;
  userUuid: string;
  login: string;
  hwidHash: string;
  source: string;
  type: string;
  signature: string;
  severity: number;
  confidence: string; // hard | soft
  status: DetectionStatus;
  reviewedBy?: string;
  reviewedAt?: string | null;
  createdAt: string;
};

// SignatureStat — агрегат детектов по сигнатуре (shadow-телеметрия, оценка FP).
export type SignatureStat = {
  signature: string;
  type: string;
  confidence: string;
  total: number;
  uniquePlayers: number;
  new: number;
  confirmed: number;
  dismissed: number;
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
  matchType: string; // substring | exact | word | regex | hash
  hashHex: string;
  severity: number;
  note: string;
  enabled: boolean;
};

export type LauncherReleaseFile = {
  id: string;
  releaseId: string;
  platform: string;
  fileName: string;
  hashSha256: string;
  size: number;
};

export type LauncherRelease = {
  id: string;
  version: string;
  changelog: string;
  mandatory: boolean;
  isActive: boolean;
  createdAt: string;
  updatedAt: string;
  files: LauncherReleaseFile[];
};
