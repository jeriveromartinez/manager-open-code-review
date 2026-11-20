#!/bin/sh
# Restauración de un backup en una base vacía, con verificación.
#
# Uso: restore.sh <archivo.dump> [base_destino]
#   archivo.dump   backup creado por backup.sh
#   base_destino   nombre de la base a crear/restaurar (por defecto qa_restore)
#
# Variables:
#   QA_DATABASE_URL  DSN de la instancia (obligatoria). La base destino se
#                    sustituye en el DSN, así que apunta al servidor, no a la
#                    base concreta.
#
# El script no toca la base en uso: siempre restaura en una base aparte para
# que se pueda comprobar antes de conmutar.
set -eu

archivo="${1:-}"
destino="${2:-qa_restore}"

if [ -z "$archivo" ] || [ ! -f "$archivo" ]; then
    echo "usage: restore.sh <archivo.dump> [base_destino]" >&2
    exit 2
fi
if [ -z "${QA_DATABASE_URL:-}" ]; then
    echo "QA_DATABASE_URL is required" >&2
    exit 1
fi

# El DSN trae la base original; se reemplaza por la de restauración.
servidor="${QA_DATABASE_URL%%\?*}"
consulta=""
case "$QA_DATABASE_URL" in
    *\?*) consulta="?${QA_DATABASE_URL#*\?}" ;;
esac
prefijo="${servidor%/*}"
url_destino="$prefijo/$destino$consulta"
url_admin="$prefijo/postgres$consulta"

if ! pg_restore --list "$archivo" >/dev/null 2>&1; then
    echo "restore: $archivo is not a readable dump" >&2
    exit 1
fi

echo "restore: recreating database $destino"
psql "$url_admin" -v ON_ERROR_STOP=1 -q \
    -c "DROP DATABASE IF EXISTS \"$destino\" WITH (FORCE)" \
    -c "CREATE DATABASE \"$destino\""

pg_restore --dbname="$url_destino" --no-owner --no-privileges --exit-on-error "$archivo"

# Verificación mínima: sin las tablas de negocio la restauración no sirve,
# aunque pg_restore haya terminado con código 0.
tablas=$(psql "$url_destino" -v ON_ERROR_STOP=1 -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'")
if [ "$tablas" -lt 1 ]; then
    echo "restore: $destino has no tables after the restore" >&2
    exit 1
fi

echo "restore: $destino restored with $tablas tables"
echo "restore: point QA_DATABASE_URL at $destino to use it, after verifying the panel"
