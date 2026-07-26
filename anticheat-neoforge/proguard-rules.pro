# ProGuard для обфускации мода P5. Подключается ОТДЕЛЬНЫМ ручным шагом после build (в
# gradle-сборку НЕ включён — `gradle build` отдаёт чистые jar-ы). Обфусцировать КАЖДЫЙ
# артефакт по отдельности:
#   proguard @proguard-rules.pro -injars build/libs/anticheat-neoforge-0.1.0-server.jar -outjars server-obf.jar
#   proguard @proguard-rules.pro -injars build/libs/anticheat-neoforge-0.1.0-client.jar -outjars client-obf.jar
# ⚠️ После обфускации ОБЯЗАТЕЛЬНО перепроверь мод в игре — обфускация Java часто ломает
# рефлексию/загрузку в рантайме, а не на компиляции.

# --- НЕ переименовывать: точки входа, которые NeoForge грузит по имени/через рефлексию ---
# Обе @Mod-точки (клиентская и серверная) — по одной на свой jar, держим обе.
-keep public class xyz.projectminecraft.anticheat.p5.P5ModClient {
    public <init>(net.neoforged.bus.api.IEventBus);
}
-keep public class xyz.projectminecraft.anticheat.p5.P5ModServer {
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
