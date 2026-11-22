#!/bin/sh
# Doble de prueba del binario `ocr` para el motor nativo.
#
# FAKE_OCR_ARGV_FILE: si se define, escribe un argumento por línea en ese archivo.
# FAKE_OCR_CALLS:     si se define, escribe una línea por invocación.
# FAKE_OCR_BACKGROUND_OUT: si se define, copia el archivo de --background-file.
# FAKE_OCR_ENV_OUT:   si se define, vuelca el entorno recibido.
# FAKE_OCR_FAIL:      si se define, falla después de escribir ese texto en stderr.
# FAKE_OCR_SLEEP:     segundos que duerme antes de responder (timeout).
# FAKE_OCR_OUTPUT:    archivo JSON alternativo con la salida nativa.

set -eu

if [ -n "${FAKE_OCR_CALLS:-}" ]; then
    printf 'call\n' >>"$FAKE_OCR_CALLS"
fi

if [ -n "${FAKE_OCR_ARGV_FILE:-}" ]; then
    printf '%s\n' "$@" >>"$FAKE_OCR_ARGV_FILE"
fi

if [ -n "${FAKE_OCR_ENV_OUT:-}" ]; then
    env >"$FAKE_OCR_ENV_OUT"
fi

if [ -n "${FAKE_OCR_BACKGROUND_OUT:-}" ]; then
    prev=""
    for arg in "$@"; do
        if [ "$prev" = "--background-file" ]; then
            cp "$arg" "$FAKE_OCR_BACKGROUND_OUT"
        fi
        prev="$arg"
    done
fi

if [ -n "${FAKE_OCR_FAIL:-}" ]; then
    printf 'fakeocr falló: %s\n' "$FAKE_OCR_FAIL" >&2
    exit 1
fi

if [ -n "${FAKE_OCR_SLEEP:-}" ]; then
    sleep "$FAKE_OCR_SLEEP"
fi

if [ -n "${FAKE_OCR_OUTPUT:-}" ] && [ -f "$FAKE_OCR_OUTPUT" ]; then
    cat "$FAKE_OCR_OUTPUT"
    exit 0
fi

cat <<'JSON'
{
  "status": "complete",
  "llm": {"provider": "nvidia", "model": "nemotron"},
  "summary": {
    "files_reviewed": 2,
    "comments": 1,
    "total_tokens": 150,
    "input_tokens": 100,
    "output_tokens": 50,
    "cache_read_tokens": 0,
    "cache_write_tokens": 0,
    "elapsed": "1s"
  },
  "comments": [
    {
      "path": "auth.go",
      "content": "Comparación insegura\n\nusar hash seguro",
      "start_line": 3,
      "end_line": 3,
      "category": "security",
      "severity": "high",
      "suggestion_code": ""
    }
  ],
  "manifest": {
    "schema_version": "ocr.run-manifest/v1",
    "terminal_state": "complete",
    "coverage": {
      "selected": [{"path": "auth.go"}, {"path": "main.go"}],
      "completed": [{"path": "auth.go"}, {"path": "main.go"}],
      "reused": [],
      "failed": [],
      "waived": [],
      "excluded": []
    }
  }
}
JSON
