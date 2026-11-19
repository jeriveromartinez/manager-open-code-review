#!/bin/sh
# Doble de prueba del binario `ocr`: registra argv y devuelve los fixtures reales.
# FAKE_OCR_ARGV_FILE: si se define, escribe un argumento por línea en ese archivo.
# FAKE_OCR_FAIL: si se define, falla después de escribir ese texto en stderr.

for arg in "$@"; do
    printf '<%s>' "$arg" >&2
done
printf '\n' >&2

if [ -n "${FAKE_OCR_ARGV_FILE:-}" ]; then
    printf '%s\n' "$@" >>"$FAKE_OCR_ARGV_FILE"
fi

if [ -n "${FAKE_OCR_FAIL:-}" ]; then
    printf 'fakeocr falló: %s\n' "$FAKE_OCR_FAIL" >&2
    exit 1
fi

subcommand="${2:-}"
case "$subcommand" in
    preview)
        dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
        cat "$dir/ocr-preview.json"
        ;;
    rule)
        dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
        cat "$dir/ocr-rule.json"
        ;;
    *)
        printf 'subcomando desconocido: %s\n' "$subcommand" >&2
        exit 2
        ;;
esac
