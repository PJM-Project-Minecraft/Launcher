/*
 * Минимальный machine-dependent заголовок JNI для Windows-сборки нативного агента
 * кросс-компиляцией под mingw-w64 на Linux. jni.h/jvmti.h (GPL+Classpath) НЕ вендорятся —
 * они копируются из локального JDK на этапе сборки (см. build-win.sh); этот файл лишь
 * заменяет платформенный $JDK/include/win32/jni_md.h, которого нет в Linux-JDK.
 */
#ifndef _JAVASOFT_JNI_MD_H_
#define _JAVASOFT_JNI_MD_H_

#define JNIEXPORT __declspec(dllexport)
#define JNIIMPORT __declspec(dllimport)
#define JNICALL __stdcall

typedef long jint;
typedef __int64 jlong;
typedef signed char jbyte;

#endif /* _JAVASOFT_JNI_MD_H_ */
