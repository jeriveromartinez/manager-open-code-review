# Identidad de la comparación base–head (#141)

## Problema

El PR guarda `head_sha` y el nombre de la rama base. Un cambio de base (retarget, push a la
base o merge de la base) mantiene el head, el evento `edited` no dispara revisión y la
deduplicación por head da por cubierto un resultado de otra comparación. El run puede
publicar hallazgos, resumen y estado de un diff que ya no existe.

## Identidad

Clave de comparación: `(head_sha, base_ref, merge_base_sha)`.

- `merge_base_sha` es la base real del diff de tres puntos (`base...head`), no la punta de
  la rama.
- `base_ref` entra en la clave porque el contexto que recibe OCR muestra la rama base.
- Persistencia: `pull_requests.base_sha` (punta publicada de la base) y
  `pull_requests.merge_base_sha`; `review_runs.base_ref` y `review_runs.base_tip_sha`.
  `review_runs.base_sha` ya guarda el merge-base resuelto y sigue siendo la referencia
  del diff y de la revisión incremental.
- Migración `00016` (después de la de #144): añade las columnas; los runs antiguos quedan
  con `base_ref` vacío y se tratan como comodín una sola vez.

## Cuándo hace falta un run nuevo

- Cambia el head (lo de siempre).
- Cambia `base_ref` aunque el merge-base sea el mismo: cambia el contexto del prompt.
- Con el mismo head y rama, `Compare` informa de un merge-base distinto (base reescrita).
- Una edición de metadatos que no altera la comparación reutiliza el resultado.

La resolución con la API `Compare` solo se hace cuando el head no cambió y la base se movió.
Si `Compare` falla, se encola una revisión nueva: nunca se reutiliza a ciegas.

## Deduplicación y publicación

- Una entrega queda cubierta solo por un run no superado cuya clave coincida con la vigente
  del PR. Cualquier run activo con otra clave se supera aunque el head coincida.
- Al terminar, el run comprueba la clave del PR: si cambió, no publica (ni resumen, ni check,
  ni estado) y el run queda `superseded`/`comparison_moved`.
- El check es único por (PR, head): dos comparaciones del mismo head comparten check, así que
  el guard de publicación es obligatorio.
- El polling resuelve la misma clave; un webhook perdido no deja un PR sin revisar.

## Cambios por archivo

- `internal/github/convert.go`: mapear `base.sha` del payload a `base_sha`.
- `internal/github/webhook.go`: disparar revisión en `edited` cuando cambie la clave.
- `internal/github/compare.go` (nuevo): resolver el merge-base con `Compare base...head`.
- `internal/store/models.go`, `store/prs.go`: persistir las columnas y limpiar
  `merge_base_sha` cuando se mueve el head o la base.
- `internal/store/review.go`: `CreateReviewRun` guarda la clave; la deduplicación y la
  supersesión pasan a ser por comparación; `PendingReviewPRs` compara claves.
- `internal/review/trigger.go`: resolver `Compare` solo para el mismo head con base movida,
  deduplicar por clave y guardar la clave al crear el run.
- `internal/review/run.go`: tras el clon, superar como `comparison_moved` si la clave
  resuelta difiere; no tocar el estado del PR cuando la clave cambió.
- `internal/publish/publish.go`: publicar solo si la clave del run coincide con la vigente.
- `internal/syncer/syncer.go`: resolver y anunciar cambios de comparación.

## Tests

- Retarget con el mismo head: run nuevo, el anterior activo se supera, el terminal antiguo
  no publica.
- La base avanza con el mismo merge-base: se reutiliza, sin run.
- La base se reescribe con el mismo head: run nuevo con diff completo, no incremental.
- Edición de metadatos: sin run.
- Webhook perdido: el polling detecta la clave nueva y encola.
- Un run antiguo termina después del retarget: no toca resumen, check ni estado.
- Fallo de `Compare`: run nuevo (conservador), nunca reutilización silenciosa.
- Filas antiguas con clave vacía siguen deduplicando.

## Riesgos

- La API `Compare` añade latencia y límite de tasa: se resuelve fuera del lock del PR y
  cualquier fallo degrada a run nuevo.
- La base puede moverse entre la resolución y el clon: el worker vuelve a comprobar la clave.
- Incluir `base_ref` provoca un run redundante al retarget a un merge-base equivalente;
  es deliberado porque cambia el contexto del prompt.
- El check compartido por (PR, head) hace que el guard de publicación no sea opcional.

## Fuera de alcance

La entrega durable y los reintentos idempotentes (#138) y la evaluación única del CI
requerido (#143) se implementan sobre esta clave, pero no aquí.
