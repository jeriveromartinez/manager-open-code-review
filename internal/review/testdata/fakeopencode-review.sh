#!/bin/sh
# Doble de prueba del binario `opencode run` usado por el orquestador de revisión.
#
# Emite un evento JSONL de texto con la salida estructurada esperada por
# ValidateOutput. Variables de entorno opcionales:
#
#   RUN_FAKE_AGENT_CALLS          archivo donde se registra una línea por invocación
#   RUN_FAKE_AGENT_FAIL           si está definida, escribe en stderr y falla
#   RUN_FAKE_AGENT_FAIL_ON_CALL   número de invocación que debe fallar (1-based)
#   RUN_FAKE_AGENT_SLEEP_ON_CALL  número de invocación que debe dormir (timeout)
#   RUN_FAKE_AGENT_OUTPUT         archivo JSON alternativo (por defecto output.json)
#
# Si junto al script hay un invalid.json, la primera invocación emite ese
# contenido (para probar el reintento por esquema inválido).

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
default_output='{"summary":"Revisión completada","coverage":{"reviewed_files":1,"skipped_files":0},"previous_findings":[],"findings":[{"path":"auth.go","severity":"major","category":"security","title":"Comparación insegura","body":"usar hash seguro","suggestion":"","confidence":"high","start_line":3,"end_line":3}]}'

calls="${RUN_FAKE_AGENT_CALLS:-}"
if [ -n "$calls" ]; then
    printf 'call\n' >>"$calls"
fi

if [ -n "${RUN_FAKE_AGENT_FAIL:-}" ]; then
    printf 'fake opencode review: provider unavailable\n' >&2
    exit 1
fi

fail_on="${RUN_FAKE_AGENT_FAIL_ON_CALL:-}"
if [ -n "$fail_on" ] && [ -n "$calls" ] && [ -f "$calls" ]; then
    n=$(wc -l <"$calls" | tr -d ' ')
    if [ "$n" -eq "$fail_on" ]; then
        printf 'fake opencode review: falló la invocación %s\n' "$n" >&2
        exit 1
    fi
fi

sleep_on="${RUN_FAKE_AGENT_SLEEP_ON_CALL:-}"
if [ -n "$sleep_on" ] && [ -n "$calls" ] && [ -f "$calls" ]; then
    n=$(wc -l <"$calls" | tr -d ' ')
    if [ "$n" -eq "$sleep_on" ]; then
        sleep 30
    fi
fi

out="${RUN_FAKE_AGENT_OUTPUT:-$script_dir/output.json}"
if [ ! -f "$out" ]; then
    raw=$default_output
else
    raw=$(cat "$out")
fi

if [ -n "$calls" ] && [ -f "$calls" ]; then
    n=$(wc -l <"$calls" | tr -d ' ')
    if [ "$n" -le 1 ] && [ -f "$script_dir/invalid.json" ]; then
        raw=$(cat "$script_dir/invalid.json")
    fi
fi

escaped=$(printf '%s' "$raw" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')
printf '{"type":"text","part":{"type":"text","text":"%s"}}\n' "$escaped"
