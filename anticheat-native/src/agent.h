#ifndef ANTICHEAT_AGENT_H
#define ANTICHEAT_AGENT_H

/*
 * Общий заголовок нативного агента: точки соприкосновения agent.c <-> guard.c.
 */

/* Дописывает событие "<type>\t<name>" в файл событий (<flagfile>.events), который
 * читает Java-агент и пересылает на бэкенд. Имя санитизируется (печатный ASCII),
 * число событий за сессию ограничено (общий лимит с ClassFileLoadHook). Потокобезопасно
 * в пределах «append в один файл»: guard-поток и hook-колбэк пишут короткими записями. */
void ac_append_event(const char *type, const char *name);

/* Запускает фоновый guard-поток анти-инжекта (поллинг загруженных модулей + непрерывный
 * anti-debug). Linux: /proc/self/maps + LD_PRELOAD/LD_AUDIT + TracerPid. Windows:
 * EnumProcessModules + IsDebuggerPresent/CheckRemoteDebuggerPresent/ProcessDebugPort.
 * На прочих ОС — no-op. Вызывать один раз из Agent_OnLoad после write_flag_file. */
void ac_guard_start(void);

#endif /* ANTICHEAT_AGENT_H */
