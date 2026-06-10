'use client';

import { useCallback, useEffect, useState } from 'react';
import { motion } from 'framer-motion';
import { api, apiUrl } from './lib/api';
import type { Detection, Stats } from './lib/types';
import { useProfileEvents } from './lib/use-sse';
import { SkeletonStats, SkeletonTable } from '../components/ui/skeleton';
import { StatsRow } from '../components/overview/stats-row';
import { EventsFeed } from '../components/overview/events-feed';

export default function OverviewPage() {
  const [stats, setStats] = useState<Stats | null>(null);
  const [detections, setDetections] = useState<Detection[] | null>(null);
  const [apiOnline, setApiOnline] = useState<boolean | null>(null);

  const refresh = useCallback(() => {
    api<Stats>('/api/admin/stats')
      .then(setStats)
      .catch(() => {});
    api<Detection[]>('/api/admin/anticheat/detections?limit=8')
      .then((list) => setDetections(list ?? []))
      .catch(() => setDetections((prev) => prev ?? []));
    fetch(`${apiUrl}/health`, { headers: { Accept: 'application/json' } })
      .then((r) => setApiOnline(r.ok))
      .catch(() => setApiOnline(false));
  }, []);

  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 60_000);
    return () => clearInterval(timer);
  }, [refresh]);

  useProfileEvents(refresh);

  const loading = stats === null || detections === null;

  return (
    <div className="flex flex-col gap-5">
      <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <h1 className="text-lg font-bold text-fg">Обзор</h1>
        <p className="text-sm text-fg-muted">Состояние системы и последние события</p>
      </motion.div>

      {loading ? (
        <div className="flex flex-col gap-5">
          <SkeletonStats />
          <SkeletonTable rows={5} cols={4} />
        </div>
      ) : (
        <motion.div
          initial={{ opacity: 0, y: 8 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.3, delay: 0.05 }}
          className="flex flex-col gap-5"
        >
          <StatsRow stats={stats} apiOnline={apiOnline} />
          <EventsFeed detections={detections} />
        </motion.div>
      )}
    </div>
  );
}
