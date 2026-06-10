'use client';

import { FormEvent, useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import { motion } from 'framer-motion';
import { Pickaxe } from 'lucide-react';
import { apiUrl, getToken, setToken } from '../lib/api';
import type { AuthUser } from '../lib/types';
import { Card } from '../../components/ui/card';
import { Field } from '../../components/ui/field';
import { Input } from '../../components/ui/input';
import { Button } from '../../components/ui/button';
import { useToast } from '../../components/ui/toast';

type LoginResponse = {
  token?: string;
  user?: AuthUser;
  message?: string;
  requiresTwoFactor?: boolean;
};

export default function LoginPage() {
  const router = useRouter();
  const toast = useToast();
  const [login, setLogin] = useState('');
  const [password, setPassword] = useState('');
  const [totp, setTotp] = useState('');
  const [needTotp, setNeedTotp] = useState(false);
  const [loading, setLoading] = useState(false);

  // Уже авторизованный админ — сразу внутрь. Raw fetch, чтобы 401 не дёргал авто-разлогин.
  useEffect(() => {
    const token = getToken();
    if (!token) return;
    fetch(`${apiUrl}/api/auth/me`, { headers: { Authorization: `Bearer ${token}` } })
      .then((r) => (r.ok ? (r.json() as Promise<AuthUser>) : null))
      .then((u) => {
        if (u?.role === 'admin') router.replace('/');
      })
      .catch(() => {});
  }, [router]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setLoading(true);
    try {
      // Raw fetch: ApiError не доносит флаг requiresTwoFactor из тела ошибки.
      let res: Response;
      try {
        res = await fetch(`${apiUrl}/api/auth/login`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ login, password, totp: totp || undefined })
        });
      } catch {
        toast('error', 'Backend недоступен');
        return;
      }
      const data = (await res.json().catch(() => ({}))) as LoginResponse;

      if (!res.ok) {
        if (data.requiresTwoFactor) {
          setNeedTotp(true);
          toast('info', 'Введите код из приложения 2FA');
        } else {
          toast('error', data.message ?? `Ошибка ${res.status}`);
        }
        return;
      }
      if (!data.token || !data.user) {
        toast('error', 'Некорректный ответ сервера');
        return;
      }
      if (data.user.role !== 'admin') {
        toast('error', 'Аккаунт не имеет прав администратора');
        return;
      }
      setToken(data.token);
      router.replace('/');
    } finally {
      setLoading(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center bg-bg px-4">
      <motion.div
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.35 }}
        className="w-full max-w-sm"
      >
        <Card className="w-full p-6">
          <div className="mb-6 flex items-center gap-3">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg border border-edge bg-surface-strong">
              <Pickaxe size={18} className="text-fg" />
            </div>
            <div>
              <h1 className="text-base font-bold text-fg">PJM Admin</h1>
              <p className="text-xs text-fg-muted">Вход в панель управления</p>
            </div>
          </div>

          <form onSubmit={submit} className="flex flex-col gap-4">
            <Field label="Логин">
              <Input
                autoFocus
                value={login}
                onChange={(e) => setLogin(e.target.value)}
                placeholder="nickname"
                autoComplete="username"
              />
            </Field>
            <Field label="Пароль">
              <Input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="••••••••"
                autoComplete="current-password"
              />
            </Field>
            {needTotp && (
              <motion.div initial={{ opacity: 0, y: -6 }} animate={{ opacity: 1, y: 0 }}>
                <Field label="Код 2FA" hint="Шестизначный код из приложения-аутентификатора">
                  <Input
                    autoFocus
                    value={totp}
                    onChange={(e) => setTotp(e.target.value)}
                    placeholder="000000"
                    inputMode="numeric"
                    maxLength={6}
                  />
                </Field>
              </motion.div>
            )}
            <Button type="submit" variant="primary" loading={loading} className="mt-1 w-full">
              Войти
            </Button>
          </form>
        </Card>
      </motion.div>
    </main>
  );
}
