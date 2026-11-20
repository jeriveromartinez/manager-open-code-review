# Backup y restauración de Postgres

Los backups viven fuera de la app: `pg_dump` habla con el servidor y nunca
necesita `QA_MASTER_KEY`, así que copiar la base no amplía el alcance de lo que
un operador puede descifrar.

## Backup programado

El perfil `backup` de `deploy/docker-compose.yml` ejecuta `deploy/backup.sh`
una vez al día a las 03:00 UTC y conserva en el volumen `backups-data` los
últimos `backup_retention` backups, un ajuste del panel en **Ajustes →
Backups** (7 por defecto).

El script lee `backup_retention` de `app_settings` con `psql` al empezar. Si no
hay fila, la consulta falla o el valor no es un entero, usa `7`: el backup ya
necesita la base para `pg_dump`, así que un fallo de lectura no debe abortar la
copia. La retención se aplica al terminar, de modo que un cambio en el panel
surte efecto en la siguiente ejecución sin reiniciar el contenedor.

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

## Copia fuera del host

El volumen `backups-data` vive en el mismo host que la base y que Docker. Si se
pierde el host, se pierden la base **y sus backups**: un backup que solo existe
en ese volumen no cubre el fallo para el que se hace. Hay que sacar los `.dump`
del volumen y llevarlos a otro sitio.

`deploy/backup-pull.sh` copia los dumps del volumen a un directorio del host.
Solo lee: no borra nada del volumen, no habla con Postgres y no necesita
`QA_DATABASE_URL`, así que se puede ejecutar tantas veces como haga falta.

```sh
# Copia a ./backups, relativo al directorio desde el que se llama.
deploy/backup-pull.sh

# O a un destino explícito.
deploy/backup-pull.sh /srv/qa-backups
```

El volumen real lo prefija el proyecto de compose (`<proyecto>_backups-data`),
así que el script no adivina el prefijo: si hay un único volumen que termina en
`_backups-data` lo usa, y si hay varios pide que se elija. Para fijarlo a mano:

```sh
# Ver el nombre real del volumen.
docker volume ls

QA_BACKUP_VOLUME=mi-proyecto_backups-data deploy/backup-pull.sh /srv/qa-backups

# Equivalente, dejando que el script componga el nombre.
QA_COMPOSE_PROJECT=mi-proyecto deploy/backup-pull.sh /srv/qa-backups
```

Solo se copian los `.dump` terminados: un `.partial` todavía se está escribiendo
y no es un backup. El script falla con código distinto de cero si no encuentra
el volumen, si no puede escribir en el destino o si el volumen no tiene ningún
`.dump`; si copia, dice cuántos archivos dejó.

La copia en el host sigue siendo el mismo host. El último paso es del operador:
sincronizar ese directorio a otra máquina o a un almacenamiento de objetos.

```sh
# A otra máquina.
rsync -av --delete /srv/qa-backups/ backup-host:/srv/qa-backups/

# O a un bucket, con la herramienta que ya use la infraestructura.
aws s3 sync /srv/qa-backups/ s3://mi-bucket/qa-backups/
```

Programa esa sincronización después del backup diario; el script no la hace por
ti.

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

La prueba mensual debe usar un dump de la copia fuera del host, no del volumen
local: solo así se demuestra que esa copia sirve cuando falte el host.
