# Backup y restauración de Postgres

Los backups viven fuera de la app: `pg_dump` habla con el servidor y nunca
necesita `QA_MASTER_KEY`, así que copiar la base no amplía el alcance de lo que
un operador puede descifrar.

## Backup programado

El perfil `backup` de `deploy/docker-compose.yml` ejecuta `deploy/backup.sh`
una vez al día a las 03:00 UTC y conserva los últimos `QA_BACKUP_RETENTION`
backups (7 por defecto) en el volumen `backups-data`.

```sh
docker compose -f deploy/docker-compose.yml --profile backup up -d backup
```

Un backup se escribe como `.partial`, se comprueba con `pg_restore --list` y
solo entonces se renombra. Si `pg_dump` falla o el dump no es legible, el
archivo parcial se borra y **los backups anteriores quedan intactos**.

Para lanzar uno a mano:

```sh
docker compose -f deploy/docker-compose.yml --profile backup \
  run --rm backup /bin/sh /scripts/backup.sh /backups
```

## Restauración

`deploy/restore.sh` nunca escribe sobre la base en uso: recrea una base nueva y
restaura ahí, de modo que el resultado se pueda inspeccionar antes de conmutar.

```sh
# 1) Restaurar en una base aparte (por defecto qa_restore).
docker compose -f deploy/docker-compose.yml --profile backup \
  run --rm --entrypoint /bin/sh backup \
  /scripts/restore.sh /backups/qa-20260101T030000Z.dump qa_restore

# 2) Comprobar los datos con la app antes de conmutar.
#    Apunta QA_DATABASE_URL a qa_restore, levanta app y worker y verifica el
#    panel: repositorios, equipos, políticas y hallazgos.

# 3) Conmutar cuando la verificación pase.
#    Actualiza QA_DATABASE_URL en deploy/.env y recrea app y worker.
docker compose -f deploy/docker-compose.yml up -d app worker
```

El script falla con código distinto de cero si el archivo no es un dump legible
o si la base restaurada queda sin tablas, así que un `&&` en un runbook detiene
la secuencia en vez de dejarte con una base vacía.

### Claves

`QA_MASTER_KEY` y `QA_WORKER_KEY` **no** están en el backup porque cifran los
secretos de la base: las credenciales de GitHub y las API keys de los perfiles
LLM. Guárdalas aparte (gestor de secretos o sobre sellado) y conserva la versión
con la que se cifró cada backup. Sin la clave correcta la base restaura pero la
app no puede descifrar las credenciales y hay que volver a introducirlas.

## Backup previo obligatorio en migraciones

Antes de aplicar una migración con `qa migrate`, toma un backup y verifica que
`pg_restore --list` lo lee. El procedimiento es el mismo de arriba; hacerlo
después de un `pg_dump` fallido no sirve de nada.

## Verificación de que el backup sirve

Un backup no verificado no es un backup. Una vez al mes, restaura el más
reciente en una base limpia con el procedimiento de arriba y confirma que la app
arranca y muestra los datos. La comprobación de `restore.sh` (al menos una
tabla) detecta los dumps truncados, pero no sustituye a esta prueba.
