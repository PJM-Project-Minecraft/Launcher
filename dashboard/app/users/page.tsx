import { Suspense } from 'react';
import { SkeletonTable } from '../../components/ui/skeleton';
import { UsersTable } from '../../components/users/users-table';

export default function UsersPage() {
  return (
    <section className="flex w-full flex-col gap-5">
      <header>
        <h1 className="text-xl font-bold">Пользователи</h1>
        <p className="mt-1 text-sm text-fg-muted">Аккаунты, роли, баны и журналы входов</p>
      </header>
      {/* useSearchParams внутри UsersTable требует Suspense-границу. */}
      <Suspense fallback={<SkeletonTable rows={8} cols={5} />}>
        <UsersTable />
      </Suspense>
    </section>
  );
}
