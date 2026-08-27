package xyz.projectminecraft.anticheat.p5;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ConcurrentHashMap;

/** Connection-scoped local lease deadlines. Backend outages do not renew a
 * lease; after the bounded grace the exact connection is returned for kick. */
final class P5LeaseTracker<C> {
    record Lease<C>(String key, C connection, String nonce, long deadlineMillis) {}

    private final long outageGraceMillis;
    private final ConcurrentHashMap<String, Lease<C>> leases = new ConcurrentHashMap<>();

    P5LeaseTracker(long outageGraceMillis) {
        this.outageGraceMillis = outageGraceMillis;
    }

    void begin(String key, C connection, long nowMillis) {
        leases.put(key, new Lease<>(key, connection, "", nowMillis + outageGraceMillis));
    }

    boolean activate(String key, C connection, String nonce, long remainingMillis, long nowMillis) {
        if (nonce.isEmpty() || remainingMillis <= 0) return false;
        return updateDeadline(key, connection, nonce, remainingMillis, nowMillis, true);
    }

    boolean renew(String key, C connection, String nonce, long remainingMillis, long nowMillis) {
        if (nonce.isEmpty() || remainingMillis <= 0) return false;
        return updateDeadline(key, connection, nonce, remainingMillis, nowMillis, false);
    }

    private boolean updateDeadline(String key, C connection, String nonce, long remainingMillis,
                                   long nowMillis, boolean allowProbation) {
        final boolean[] renewed = {false};
        leases.computeIfPresent(key, (ignored, current) -> {
            if (current.connection() != connection) return current;
            if (!allowProbation && !current.nonce().equals(nonce)) return current;
            if (allowProbation && !current.nonce().isEmpty() && !current.nonce().equals(nonce)) return current;
            renewed[0] = true;
            long boundedRemaining = Math.min(remainingMillis, outageGraceMillis);
            return new Lease<>(key, connection, nonce, nowMillis + boundedRemaining);
        });
        return renewed[0];
    }

    Lease<C> current(String key) {
        return leases.get(key);
    }

    List<Lease<C>> snapshot() {
        return new ArrayList<>(leases.values());
    }

    void remove(String key, C connection) {
        leases.computeIfPresent(key, (ignored, current) -> current.connection() == connection ? null : current);
    }

    List<Lease<C>> expired(long nowMillis) {
        List<Lease<C>> expired = new ArrayList<>();
        for (Lease<C> lease : leases.values()) {
            if (nowMillis > lease.deadlineMillis() && leases.remove(lease.key(), lease)) {
                expired.add(lease);
            }
        }
        return expired;
    }

    void clear() {
        leases.clear();
    }
}
