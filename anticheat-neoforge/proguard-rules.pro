# ProGuard для обфускации мода P5. Подключается отдельным шагом после build (proguard
# -injars build/libs/pjmac.jar ...) либо через gradle-плагин. ⚠️ После обфускации
# ОБЯЗАТЕЛЬНО перепроверь мод в игре — обфускация Java часто ломает рефлексию/загрузку
# в рантайме, а не на компиляции.

# --- НЕ переименовывать: точки входа, которые NeoForge грузит по имени/через рефлексию ---
-keep public class xyz.projectminecraft.anticheat.p5.P5Mod {
    public <init>(net.neoforged.bus.api.IEventBus);
}
# Payload-record'ы и их TYPE/CODEC читаются каналом по имени.
-keep class xyz.projectminecraft.anticheat.p5.P5Payloads { *; }
-keep class xyz.projectminecraft.anticheat.p5.P5Payloads$* { *; }

# --- NeoForge/Minecraft из рантайма сервера — как библиотеки, не включать и не трогать ---
-keep class net.minecraft.** { *; }
-keep class net.neoforged.** { *; }
-dontwarn net.**
-dontwarn com.google.gson.**

# --- Обфусцируем внутреннюю логику (P5Crypto/Config/Server/Client) ---
# Имена методов/полей мешаются. Строк ProGuard core не шифрует (это DexGuard/плагины);
# но чувствительные значения (ANTICHEAT_P5_SECRET, LAUNCHER_API) берутся из ENV и в jar
# не попадают — обфускация строк здесь не критична. Путь эндпоинта не секрет.
-keepattributes RuntimeVisibleAnnotations,RuntimeVisibleParameterAnnotations,Signature,InnerClasses,EnclosingMethod
-optimizationpasses 3
-dontusemixedcaseclassnames
-repackageclasses 'x'

# --- CJK-манглинг имён: классы/методы/поля переименовываются в иероглифы (cjk-dict.txt) ---
# Декомпилятор показывает 私.四() вместо checkKillaura(); имена по .class валидны (JVM
# разрешает Unicode-идентификаторы). Это RENAMING, не защита строк — строки шифруются
# отдельно (в agent.jar). Словарь общий для имён всех видов: коллизий ProGuard избегает сам.
-obfuscationdictionary cjk-dict.txt
-classobfuscationdictionary cjk-dict.txt
-packageobfuscationdictionary cjk-dict.txt
