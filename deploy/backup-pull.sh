#!/bin/sh
# Copia de los backups del volumen al host, para que sobrevivan a su pérdida.
#
# Uso: backup-pull.sh [destino]
#   destino  directorio del host donde dejar los .dump (por defecto ./backups)
#
# Variables:
#   QA_BACKUP_VOLUME    nombre real del volumen (por defecto se resuelve solo)
#   QA_COMPOSE_PROJECT  proyecto de compose; el volumen es <proyecto>_backups-data
#
# Un backup que solo existe en el mismo host que la base desaparece con el
# host. Este script saca los .dump del volumen sin borrar nada: solo lee, así
# que se puede repetir tantas veces como haga falta.
set -eu

imagen="postgres:17"
destino="${1:-./backups}"

if ! command -v docker >/dev/null 2>&1; then
    echo "backup-pull: docker is not available" >&2
    exit 1
fi

if [ -n "${QA_BACKUP_VOLUME:-}" ]; then
    volumen="$QA_BACKUP_VOLUME"
elif [ -n "${QA_COMPOSE_PROJECT:-}" ]; then
    volumen="${QA_COMPOSE_PROJECT}_backups-data"
else
    # El prefijo lo pone compose y cambia con el proyecto, así que en vez de
    # adivinarlo se busca el único volumen que termina en _backups-data.
    candidatos=$(docker volume ls --format '{{.Name}}' | grep '_backups-data$' || true)
    total=$(printf '%s\n' "$candidatos" | sed '/^$/d' | wc -l)
    if [ "$total" -eq 1 ]; then
        volumen="$candidatos"
    elif [ "$total" -eq 0 ]; then
        echo "backup-pull: no *_backups-data volume found; set QA_BACKUP_VOLUME" >&2
        echo "backup-pull: list the volumes with: docker volume ls" >&2
        exit 1
    else
        echo "backup-pull: several volumes match, set QA_BACKUP_VOLUME to one of:" >&2
        printf '%s\n' "$candidatos" | sed '/^$/d; s/^/  /' >&2
        exit 1
    fi
fi

if ! docker volume inspect "$volumen" >/dev/null 2>&1; then
    echo "backup-pull: volume $volumen is not usable; check the docker daemon and: docker volume ls" >&2
    exit 1
fi

if ! mkdir -p "$destino" || [ ! -d "$destino" ] || [ ! -w "$destino" ]; then
    echo "backup-pull: cannot write to destination $destino" >&2
    exit 1
fi

# Un bind mount necesita ruta absoluta aunque el usuario pase una relativa.
destino_abs=$(cd "$destino" && pwd -P)

# El glob solo ve *.dump: un *.partial se está escribiendo todavía y no es un
# backup, así que nunca debe aparecer en el destino.
#
# El recuento se lee de un manifiesto y no de la salida de `docker run`: cuando
# el daemon se alcanza por un proxy TCP, la salida del contenedor puede quedar
# vacía y el script diría "0 archivos" después de haber copiado correctamente.
manifiesto=".backup-pull.$$"
# shellcheck disable=SC2016  # las variables las expande el sh del contenedor
guion='
set -eu
n=0
for origen in /backups/*.dump; do
    [ -f "$origen" ] || continue
    nombre=${origen##*/}
    cp -p "$origen" "/host/$nombre"
    printf "%s\n" "$nombre" >> "/host/$1"
    n=$((n + 1))
done
[ "$n" -gt 0 ] || exit 3
'

codigo=0
docker run --rm \
    -v "$volumen:/backups:ro" \
    -v "$destino_abs:/host" \
    "$imagen" sh -c "$guion" backup-pull "$manifiesto" || codigo=$?

listado="$destino_abs/$manifiesto"
if [ -f "$listado" ]; then
    copiados=$(sed '/^$/d' "$listado")
    rm -f "$listado"
else
    copiados=""
fi

if [ "$codigo" -eq 3 ]; then
    echo "backup-pull: no .dump file in volume $volumen" >&2
    echo "backup-pull: run deploy/backup.sh first" >&2
    exit 1
fi
if [ "$codigo" -ne 0 ]; then
    echo "backup-pull: docker failed (exit $codigo) reading $volumen" >&2
    exit 1
fi

# Si no hay manifiesto, el contenedor escribió en otro sitio que el destino
# indicado; decir "0 archivos" ahí daría por buena una copia que no existe.
if [ -z "$copiados" ]; then
    echo "backup-pull: docker wrote nothing to $destino_abs" >&2
    echo "backup-pull: the destination must be a path the docker daemon can mount" >&2
    exit 1
fi

printf '%s\n' "$copiados" | sed 's/^/backup-pull: copied /'
total=$(printf '%s\n' "$copiados" | sed '/^$/d' | wc -l | tr -d ' ')
echo "backup-pull: $total files copied to $destino_abs"
echo "backup-pull: copy $destino_abs off the host to make it a real backup"
