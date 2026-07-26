package xyz.projectminecraft.anticheat;

import java.util.concurrent.atomic.AtomicInteger;

/**
 * Без JUnit (у агента нет зависимостей вне JDK): простой main с проверками.
 * Регрессия «Недействительная сессия»: фоновый тред агента тихо умирал, и сессия
 * гасла. guardedIteration — ядро устойчивости: тело-итерация не должна ронять цикл
 * НИКАКИМ Throwable (в т.ч. Error, который старый catch(Exception) пропускал).
 */
public final class AgentResilienceTest {
    public static void main(String[] args) {
        int failures = 0;

        // 1. RuntimeException из тела поглощается, помечается упавшим, onError вызван один раз.
        AtomicInteger errs = new AtomicInteger();
        boolean fell = Agent.guardedIteration(
            () -> { throw new RuntimeException("boom"); },
            (cls, t) -> errs.incrementAndGet());
        if (!fell) { System.err.println("FAIL: упавшее тело должно вернуть true"); failures++; }
        if (errs.get() != 1) { System.err.println("FAIL: onError должен вызваться ровно один раз"); failures++; }

        // 2. Error (не Exception) тоже поглощается — именно его старый catch(Exception) пропускал.
        boolean fellErr = Agent.guardedIteration(
            () -> { throw new StackOverflowError("deep"); },
            (cls, t) -> {});
        if (!fellErr) { System.err.println("FAIL: Error должен поглощаться, а не убивать цикл"); failures++; }

        // 3. Нормальное тело — false, onError не зовётся.
        AtomicInteger errs2 = new AtomicInteger();
        boolean ok = Agent.guardedIteration(() -> {}, (cls, t) -> errs2.incrementAndGet());
        if (ok) { System.err.println("FAIL: нормальное тело должно вернуть false"); failures++; }
        if (errs2.get() != 0) { System.err.println("FAIL: onError не должен зваться при успехе"); failures++; }

        // 4. Сбой самого onError не пробрасывается (отчёт о причине не должен ронять цикл).
        boolean stillFell = Agent.guardedIteration(
            () -> { throw new RuntimeException("x"); },
            (cls, t) -> { throw new RuntimeException("reporter boom"); });
        if (!stillFell) { System.err.println("FAIL: сбой onError не должен ломать guardedIteration"); failures++; }

        // 5. Обфускация строк (O.d): декодер обязан вернуть ТОЧНЫЕ маркеры, иначе детект
        //    молча ломается (список читов не совпадёт ни с одним классом). Проверяем, что
        //    обфусцированный DEFAULT_MARKERS содержит ключевые имена в исходном виде.
        for (String want : new String[]{"wurst", "killaura", "meteorclient", "huzuni"}) {
            if (!Agent.DEFAULT_MARKERS.contains(want)) {
                System.err.println("FAIL: обфускация сломала маркер '" + want + "'");
                failures++;
            }
        }
        if (Agent.DEFAULT_MARKERS.size() != 10) {
            System.err.println("FAIL: ожидалось 10 маркеров, получено " + Agent.DEFAULT_MARKERS.size());
            failures++;
        }

        if (failures > 0) {
            System.err.println(failures + " проверок упало");
            System.exit(1);
        }
        System.out.println("AgentResilienceTest: OK (5 проверок)");
    }
}
