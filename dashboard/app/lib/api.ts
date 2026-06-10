// Единый API-клиент админки: токен, ошибки, авто-разлогин по 401.

export const apiUrl = (process.env.NEXT_PUBLIC_API_URL ?? 'http://127.0.0.1:8080').replace(/\/$/, '');
export const tokenKey = 'launcher.admin.token';

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
  }
}

export function getToken(): string | null {
  if (typeof window === 'undefined') return null;
  return window.localStorage.getItem(tokenKey);
}

export function setToken(token: string) {
  window.localStorage.setItem(tokenKey, token);
}

export function clearToken() {
  window.localStorage.removeItem(tokenKey);
}

type Options = {
  method?: string;
  body?: unknown;
  /** false — не подставлять Bearer-токен и не разлогинивать по 401 (логин-форма). */
  auth?: boolean;
};

export async function api<T = unknown>(path: string, options: Options = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (options.auth !== false) {
    const token = getToken();
    if (token) headers.Authorization = `Bearer ${token}`;
  }

  let response: Response;
  try {
    response = await fetch(`${apiUrl}${path}`, {
      method: options.method ?? 'GET',
      headers,
      body: options.body !== undefined ? JSON.stringify(options.body) : undefined
    });
  } catch {
    throw new ApiError('Backend недоступен', 0);
  }

  if (response.status === 401 && options.auth !== false) {
    clearToken();
    if (typeof window !== 'undefined') window.location.href = '/login';
    throw new ApiError('Сессия истекла', 401);
  }

  if (response.status === 204) return undefined as T;

  const data = (await response.json().catch(() => ({}))) as { message?: string };
  if (!response.ok) {
    throw new ApiError(data.message ?? `Ошибка ${response.status}`, response.status);
  }
  return data as T;
}

export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : 'Неизвестная ошибка';
}
