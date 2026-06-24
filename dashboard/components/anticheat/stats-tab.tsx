'use client';

import { BarChart3 } from 'lucide-react';
import type { SignatureStat } from '../../app/lib/types';
import { Table, Th, Td } from '../ui/table';
import { Badge } from '../ui/badge';
import { SkeletonTable } from '../ui/skeleton';
import { EmptyState } from '../ui/empty-state';

function confidenceTone(confidence: string): 'danger' | 'default' {
  return confidence === 'hard' ? 'danger' : 'default';
}

// Эвристика «вероятный ложняк»: много срабатываний на многих игроков, но оператор
// ничего не подтвердил, а что-то отклонил. Такую сигнатуру надо сузить, а не банить.
function looksFalsePositive(s: SignatureStat): boolean {
  return s.total >= 5 && s.confirmed === 0 && s.dismissed > 0;
}

export function StatsTab({ stats, loading }: { stats: SignatureStat[]; loading: boolean }) {
  if (loading) {
    return <SkeletonTable rows={6} cols={8} />;
  }
  if (stats.length === 0) {
    return <EmptyState icon={BarChart3} title="Статистики пока нет" hint="За выбранный период детектов не было." />;
  }
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-fg-muted">
        Распределение детектов по сигнатурам за 7 дней. Много срабатываний на многих игроков при нуле
        подтверждённых — кандидат в ложняки: сузьте сигнатуру (match_type) или отключите, прежде чем включать
        авто-бан.
      </p>
      <Table>
        <thead>
          <tr>
            <Th>Сигнатура</Th>
            <Th>Тип</Th>
            <Th>Уверенность</Th>
            <Th>Срабатываний</Th>
            <Th>Игроков</Th>
            <Th>Новых</Th>
            <Th>Подтв.</Th>
            <Th>Откл.</Th>
          </tr>
        </thead>
        <tbody>
          {stats.map((s, i) => (
            <tr key={`${s.type}:${s.signature}:${i}`}>
              <Td className="font-medium">
                {s.signature || '—'}
                {looksFalsePositive(s) && (
                  <Badge tone="warn" className="ml-2">
                    вероятный ложняк
                  </Badge>
                )}
              </Td>
              <Td className="text-fg-secondary">{s.type}</Td>
              <Td>
                <Badge tone={confidenceTone(s.confidence)}>{s.confidence || '—'}</Badge>
              </Td>
              <Td className="text-fg-secondary">{s.total}</Td>
              <Td className="text-fg-secondary">{s.uniquePlayers}</Td>
              <Td className="text-fg-muted">{s.new}</Td>
              <Td className="text-fg-secondary">{s.confirmed}</Td>
              <Td className="text-fg-muted">{s.dismissed}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
    </div>
  );
}
