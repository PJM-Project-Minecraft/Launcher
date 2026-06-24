'use client';

import { useCallback, useEffect, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import type { AccountBan, CheatSignature, Detection, HwidBan, SignatureStat } from '../lib/types';
import { Tabs } from '../../components/ui/tabs';
import { useToast } from '../../components/ui/toast';
import { DetectionsTab } from '../../components/anticheat/detections-tab';
import { BansTab } from '../../components/anticheat/bans-tab';
import { SignaturesTab } from '../../components/anticheat/signatures-tab';
import { StatsTab } from '../../components/anticheat/stats-tab';

export default function AnticheatPage() {
  const toast = useToast();
  const [tab, setTab] = useState('detections');
  const [loading, setLoading] = useState(true);

  const [detections, setDetections] = useState<Detection[]>([]);
  const [accountBans, setAccountBans] = useState<AccountBan[]>([]);
  const [hwidBans, setHwidBans] = useState<HwidBan[]>([]);
  const [signatures, setSignatures] = useState<CheatSignature[]>([]);
  const [stats, setStats] = useState<SignatureStat[]>([]);

  const reload = useCallback(async () => {
    try {
      const [det, ab, hb, sigs, st] = await Promise.all([
        api<Detection[]>('/api/admin/anticheat/detections?limit=200'),
        api<AccountBan[]>('/api/admin/anticheat/bans/account'),
        api<HwidBan[]>('/api/admin/anticheat/bans/hwid'),
        api<CheatSignature[]>('/api/admin/anticheat/signatures'),
        api<SignatureStat[]>('/api/admin/anticheat/stats?days=7')
      ]);
      setDetections(det ?? []);
      setAccountBans(ab ?? []);
      setHwidBans(hb ?? []);
      setSignatures(sigs ?? []);
      setStats(st ?? []);
    } catch (e) {
      toast('error', errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [toast]);

  useEffect(() => {
    void reload();
  }, [reload]);

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-bold">Античит</h1>
        <p className="mt-0.5 text-sm text-fg-muted">Детекты, баны и сигнатуры читов</p>
      </div>

      <Tabs
        items={[
          { key: 'detections', label: 'Детекты', badge: detections.length },
          { key: 'bans', label: 'Баны', badge: accountBans.length + hwidBans.length },
          { key: 'signatures', label: 'Сигнатуры', badge: signatures.length },
          { key: 'stats', label: 'Статистика', badge: stats.length }
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === 'detections' && <DetectionsTab detections={detections} loading={loading} onReload={reload} />}
      {tab === 'bans' && (
        <BansTab accountBans={accountBans} hwidBans={hwidBans} loading={loading} onReload={reload} />
      )}
      {tab === 'signatures' && <SignaturesTab signatures={signatures} loading={loading} onReload={reload} />}
      {tab === 'stats' && <StatsTab stats={stats} loading={loading} />}
    </div>
  );
}
