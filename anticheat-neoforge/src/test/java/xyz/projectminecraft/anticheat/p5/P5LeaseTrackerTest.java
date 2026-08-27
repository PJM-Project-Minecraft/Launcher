package xyz.projectminecraft.anticheat.p5;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.*;

final class P5LeaseTrackerTest {
    @Test
    void validRenewalExtendsLocalGraceAndLongOutageExpires() {
        P5LeaseTracker<Object> leases = new P5LeaseTracker<>(180_000L);
        Object connection = new Object();
        leases.begin("uuid", connection, 1_000L);
        assertTrue(leases.activate("uuid", connection, "nonce", 180_000L, 1_000L));
        assertTrue(leases.renew("uuid", connection, "nonce", 180_000L, 30_000L));
        assertTrue(leases.expired(209_999L).isEmpty());
        assertEquals(1, leases.expired(210_001L).size());
    }

    @Test
    void oldConnectionCannotRenewOrExpireReplacement() {
        P5LeaseTracker<Object> leases = new P5LeaseTracker<>(180_000L);
        Object oldConnection = new Object();
        Object replacement = new Object();
        leases.begin("uuid", oldConnection, 0L);
        leases.begin("uuid", replacement, 100_000L);
        assertTrue(leases.activate("uuid", replacement, "new", 180_000L, 100_000L));

        assertFalse(leases.activate("uuid", oldConnection, "old", 180_000L, 120_000L));
        assertFalse(leases.renew("uuid", oldConnection, "old", 180_000L, 120_000L));
        assertTrue(leases.expired(180_001L).isEmpty());
        assertSame(replacement, leases.current("uuid").connection());
    }

    @Test
    void backendRemainingTimeCannotResetTheFullGrace() {
        P5LeaseTracker<Object> leases = new P5LeaseTracker<>(180_000L);
        Object connection = new Object();
        leases.begin("uuid", connection, 0L);
        assertTrue(leases.activate("uuid", connection, "nonce", 45_000L, 100_000L));
        assertTrue(leases.expired(144_999L).isEmpty());
        assertEquals(1, leases.expired(145_001L).size());
    }
}
