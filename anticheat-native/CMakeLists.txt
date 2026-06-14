# Кроссплатформенная сборка нативного JVMTI-агента античита.
#   Linux:   cmake -B build && cmake --build build  →  libanticheat.so
#   Windows: cmake -B build (Visual Studio/MinGW) && cmake --build build --config Release  →  anticheat.dll
# Требуется установленный JDK; CMake находит его через find_package(JNI) или JAVA_HOME.
cmake_minimum_required(VERSION 3.15)
# CMP0091 NEW — управлять рантаймом MSVC через свойство MSVC_RUNTIME_LIBRARY (см. ниже).
# Ставится до project(), т.к. влияет на пробинг компилятора.
cmake_policy(SET CMP0091 NEW)
project(anticheat_native C)

set(CMAKE_C_STANDARD 11)

# Каталоги заголовков JNI/JVMTI: сначала пробуем JAVA_HOME, затем find_package(JNI).
if(DEFINED ENV{JAVA_HOME} AND EXISTS "$ENV{JAVA_HOME}/include/jvmti.h")
    set(JNI_INCLUDE_DIRS "$ENV{JAVA_HOME}/include")
    if(WIN32)
        list(APPEND JNI_INCLUDE_DIRS "$ENV{JAVA_HOME}/include/win32")
    elseif(APPLE)
        list(APPEND JNI_INCLUDE_DIRS "$ENV{JAVA_HOME}/include/darwin")
    else()
        list(APPEND JNI_INCLUDE_DIRS "$ENV{JAVA_HOME}/include/linux")
    endif()
else()
    find_package(JNI REQUIRED)
endif()

add_library(anticheat SHARED src/agent.c src/guard.c)
target_include_directories(anticheat PRIVATE ${JNI_INCLUDE_DIRS})

# guard.c: на Windows нужен psapi (EnumProcessModules), на Linux — pthread.
if(WIN32)
    target_link_libraries(anticheat PRIVATE psapi)
    # Статическая линковка CRT под MSVC (/MT): DLL не зависит от VC++ Redist
    # (vcruntime140.dll/UCRT) и грузится на чистой Windows — как mingw-сборка с -static.
    set_property(TARGET anticheat PROPERTY MSVC_RUNTIME_LIBRARY "MultiThreaded")
else()
    find_package(Threads REQUIRED)
    target_link_libraries(anticheat PRIVATE Threads::Threads)
endif()

# Strip символов (анти-RE): убираем имена внутренних функций. MSVC использует иной механизм.
if(NOT MSVC)
    target_link_options(anticheat PRIVATE -s)
endif()

# Имя выходного файла: libanticheat.so / anticheat.dll / libanticheat.dylib.
set_target_properties(anticheat PROPERTIES PREFIX "" OUTPUT_NAME "anticheat")
if(NOT WIN32)
    set_target_properties(anticheat PROPERTIES PREFIX "lib")
endif()
