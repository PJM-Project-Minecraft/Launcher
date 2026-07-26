package xyz.projectminecraft.anticheat;

/**
 * Обфускация строк — Java-аналог Rust-макроса obfstr!. Чувствительные литералы (список
 * маркеров читов) не лежат в jar открытым текстом: `strings`/`javap` показывают только
 * int-массивы, а не «wurst/killaura/...». Это НЕ криптография (ключ в том же jar), а
 * подъём планки против тривиального RE — ровно как obfstr! на стороне лаунчера.
 *
 * Кодировка: char[i] ^ ((KEY + i) & 0xFFFF). Здесь только декод; массив генерится так:
 *   python3 -c 'K=0x5C7A; s="killaura";
 *     print([ (ord(c)^((K+i)&0xFFFF)) for i,c in enumerate(s) ])'
 */
final class O {
    private O() {}

    private static final int KEY = 0x5C7A;

    /** Декодирует массив, полученный XOR-обфускацией со сдвигом по позиции. */
    static String d(int[] e) {
        char[] c = new char[e.length];
        for (int i = 0; i < e.length; i++) {
            c[i] = (char) (e[i] ^ ((KEY + i) & 0xFFFF));
        }
        return new String(c);
    }
}
