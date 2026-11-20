#!/bin/sh
# Backup programado de Postgres con retención y verificación.
#
# Uso: backup.sh [destino]
#   destino  directorio donde dejar el .dump (por defecto /backups)
#
# Variables:
#   QA_DATABASE_URL   DSN de la base (obligatoria). De ahí sale también
#                     backup_retention, el ajuste del panel (7 si no se puede leer).
#
# El dump se escribe en un archivo temporal y solo se renombra cuando
# pg_restore puede leerlo, así un backup truncado nunca reemplaza a uno bueno.
set -eu

destino="${1:-/backups}"

if [ -z "${QA_DATABASE_URL:-}" ]; then
    echo "QA_DATABASE_URL is required" >&2
    exit 1
fi

# The retention is a panel setting, but a failed read must not abort the
# backup: the dump below already needs the database, so the only thing a
# missing row can cost is the configured value, not tonight's copy.
retencion=$(psql "$QA_DATABASE_URL" -tAc \
    "SELECT value FROM app_settings WHERE key = 'backup_retention'" 2>/dev/null || true)
retencion=$(printf '%s' "$retencion" | tr -d '[:space:]')
case "$retencion" in
    ''|*[!0-9]*) retencion=7 ;;
esac

mkdir -p "$destino"

marca=$(date -u +%Y%m%dT%H%M%SZ)
final="$destino/qa-$marca.dump"
parcial="$final.partial"

trap 'rm -f "$parcial"' EXIT INT TERM

echo "backup: dumping $marca"
pg_dump --dbname="$QA_DATABASE_URL" --format=custom --no-owner --no-privileges --file="$parcial"

# Verificar antes de publicar: un dump que pg_restore no puede listar no sirve
# para restaurar, y descubrirlo en el momento de la restauración es demasiado
# tarde.
if ! pg_restore --list "$parcial" >/dev/null 2>&1; then
    echo "backup: pg_restore cannot read the dump, keeping the previous one" >&2
    exit 1
fi

mv "$parcial" "$final"
echo "backup: wrote $final ($(wc -c <"$final") bytes)"

# Retención: conservar los N más recientes por nombre, que ordena por fecha.
ls -1t "$destino"/qa-*.dump 2>/dev/null | tail -n +"$((retencion + 1))" | while IFS= read -r viejo; do
    rm -f "$viejo"
    echo "backup: removed $viejo"
done
