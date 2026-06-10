'use client';

import { motion } from 'framer-motion';
import { Newspaper } from 'lucide-react';
import { EmptyState } from '../../components/ui/empty-state';

export default function NewsPage() {
  return (
    <motion.div initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
      <EmptyState
        icon={Newspaper}
        title="Раздел в разработке"
        hint="Публикация новостей появится позже. Сейчас новости подтягиваются лаунчером из Telegram-канала напрямую."
      />
    </motion.div>
  );
}
