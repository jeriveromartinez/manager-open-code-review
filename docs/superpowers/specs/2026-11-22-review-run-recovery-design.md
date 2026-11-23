# Runs cortados: tokens desconocidos y watchdog con latido

Issues: #129 y #130. Base: main con #131 (OCR entrega resultado parcial antes
del corte).

## Intención

Un run cortado no debe mentir sobre dos cosas: cuánto consumió y por qué se
cortó. Hoy el panel muestra «Tokens 0» aunque OCR hizo llamadas, y el watchdog
afirma que «el worker desapareció» sin comprobar que hubiera muerto.

## Objetivos

- Guardar como `NULL` las columnas de tokens cuando no se parseó un resultado
  de OCR, y mostrar «sin datos» (no ceros) en el detalle del PR.
- Excluir esos runs de los promedios de consumo.
- Distinguir, con un latido del worker, entre «el worker dejó de responder» y
  «la revisión superó el tiempo máximo».
- Cancelar en el worker el run que el watchdog da por agotado.
- Validar un margen mínimo entre `review_stale_after` y `review_timeout`, y
  documentar qué hace cada ajuste.

## No objetivos

- Reanudar la sesión de OCR (`--resume`) en un reintento.
- Cambiar los valores por defecto de tiempo.
- Exponer el latido en la API o en el panel.

## Enfoques considerados

### #129

1. **Flag explícito `tokensKnown`** (elegido): el pipeline sabe si parseó OCR;
   `CompleteReviewRun` escribe `NULL` cuando es falso. Claro y sin depender de
   que el total sea cero (un cero medido es un dato real).
2. Detectar «desconocido» por `summary == ""`: acopla la persistencia al texto
   del resumen y falla si OCR devuelve un resumen sin cifras.
3. Punteros en `RunUsage`: correcto, pero cambia todos los llamadores y mezcla
   «hay datos» con «el valor es nulo».

### #130

1. **Columna `heartbeat_at`** (elegida): marca explícita por run, migración
   pequeña y consulta directa.
2. Reutilizar `phase_started_at`: se pisa en cada cambio de fase y no late
   durante una fase larga; no sirve como señal de vida.
3. Mirar `updated_at`: cambia por motivos ajenos al run y no distingue un
   worker vivo de uno colgado.

La cancelación en el worker se hace con la propia escritura del latido: si el
run ya no está `running`, el runner cancela su contexto. No hace falta un canal
nuevo entre procesos.

## Decisiones

- Latido cada minuto; silencio de 3 minutos (tres latidos perdidos) se
  considera worker sin respuesta.
- Si hay latido vivo, el mensaje habla del tiempo máximo (`review_stale_after`).
  Si no lo hay, dice que el worker dejó de responder y que puede ser un reinicio
  o una caída.
- El código de error sigue siendo `interrupted`; solo cambia el mensaje.
- Margen mínimo entre los dos ajustes: 5 minutos, suficientes para clonar y
  publicar después del corte de OCR.
- En el panel, un `total_tokens` nulo con `error_code` no vacío en un run ya
  terminado es un run cortado. Un run en cola o en curso aún no ha informado su
  consumo, y sin `error_code` la fila es anterior al registro de consumo.

## Alcance

- `internal/store`: migración `00013`, modelo, `ClaimReviewRun`,
  `HeartbeatReviewRun`, `CompleteReviewRun`, `InterruptStaleRuns` y agregados.
- `internal/review`: `run.go` (flag de tokens, latido y cancelación) y
  `watchdog.go` (política y mensajes).
- `internal/settings`: margen y textos de Ajustes.
- `web`: mensaje del detalle del PR.
- Tests en los paquetes anteriores.

## Entrega

Dos ramas y dos PRs secuenciales, una por issue, cada uno con `Closes #N`.
