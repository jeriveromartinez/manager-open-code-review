# Auto-triage de hallazgos de OCR

Fecha: 2026-11-23
Estado: aprobado el enfoque; pendiente de plan de implementación.

## Problema

Tras una revisión, el equipo recibe hallazgos de OCR y un humano tiene que
confirmarlos o descartarlos uno a uno desde el panel o con `/qa`. En la cola
real de `ready_for_human` había 77 hallazgos abiertos sin triage (28 `minor`,
49 `nit`) repartidos en 10 PRs. El umbral bloqueante de la política es `major`,
así que esos hallazgos no frenan el PR pero sí consumen tiempo de QA.

Objetivo: que OCR haga la mayor parte del triage. Descartar los falsos
positivos, confirmar los reales y publicar en el PR solo lo que de verdad hay
que mirar, dejando al humano únicamente los casos dudosos.

## Qué hace hoy el pipeline

1. `ocr review --from <base> --to <head> --format json` produce comentarios
   (`path`, `content`, `existing_code`, `suggestion_code`, líneas, categoría,
   severidad). No emite `confidence`.
2. `nativeFindings` los convierte a `AgentFinding`; `Reconcile` los casa por
   fingerprint y `CompleteReviewRun` los upserta con `status = open`.
3. El run pasa a `succeeded` (o `inconclusive`), el PR a `ready_for_human` si
   no hay hallazgo abierto >= `blocking_threshold`, y se encola la publicación.
4. Un QA/lead confirma (`status = open`, `triage_verdict = confirmed`) o
   descarta (`status = withdrawn`, `triage_verdict = false_positive`) desde el
   panel (`internal/api/findings.go`) o con `/qa dismiss|confirm`.

Hechos verificados que condicionan el diseño:

- OCR 1.14.1 (1.14.2 disponible) ya trae un post-filtro LLM de comentarios;
  el worker no pasa `--no-filter`, así que los 77 hallazgos ya lo pasaron.
- `ocr review --analysis` devuelve decisiones de mantenedor en **prosa**, no un
  veredicto estructurado por hallazgo. No sirve como auto-triage por sí solo.
- Las sesiones de OCR viven dentro del clon temporal; el manager parsea
  `session_id` pero no lo guarda. Al terminar el run el clon se borra.
- `ocr scan` acepta `--path`, `--rule` y `--background-file`: permite una
  segunda pasada focalizada sobre los archivos citados, no sobre todo el PR.
- `upsertFinding` escribe `status` en cada run: un hallazgo descartado que OCR
  vuelve a reportar se reabre. Hoy no hay distinción entre triage humano y de
  máquina.

## Enfoque elegido

Dos pasadas de OCR y una política determinista:

1. **Pasada 1 (sin cambios):** `ocr review` sobre el diff.
2. **Pasada 2 verificadora, solo sobre los archivos con hallazgos:** un
   `ocr scan` con una regla estricta y un contexto que contiene los hallazgos.
   OCR vuelve a mirar ese código y responde un veredicto por hallazgo, con
   evidencia citada.
3. El manager cruza la respuesta con los hallazgos del run y aplica la
   política: confirmar, descartar (solo `minor`/`nit`) o dejar para el humano.
4. Publica un comentario curado en el PR y audita cada decisión.

La verificación la hace el mismo motor de OCR, con su toolset y sus reglas. El
manager orquesta y valida evidencia; no reimplementa revisión ni añade un
cliente LLM nuevo.

### Alternativas descartadas

- **Solo `--analysis`:** prosa, opcional y no por hallazgo. No permite aplicar
  una política ni auditar un veredicto.
- **Segunda revisión completa + `ocr session compare`:** el consenso es bueno,
  pero duplica el coste de cada PR y obliga a persistir sesiones. Se deja como
  fallback futuro para hallazgos `major`.
- **Solo reglas deterministas (grep/AST/lint):** barato y auditable, pero solo
  cubre lo mecánicamente demostrable. Se usa como pre-check, no como núcleo.
- **Verificador LLM fuera de OCR:** duplica motor, credenciales y prompts.

## Diseño

### 1. Fase `verifying` dentro del run

La verificación corre dentro de `execute`, en el mismo clon y antes de que el
run se marque terminal:

1. `Reconcile` produce los hallazgos del run.
2. Se pone `phase = verifying` y se lanza `ocr scan`.
3. Se calculan las decisiones y se vuelve a poner `phase = finishing`.
4. `CompleteReviewRun` sigue siendo una única escritura al final, con el estado
   terminal, los hallazgos, los tokens de OCR y los de la verificación.

No se parte `CompleteReviewRun`: mientras verifica, el run sigue `running`, el
panel muestra la fase y el heartbeat y el watchdog siguen aplicando. La
publicación no arranca hasta que el run es terminal, así que nunca ve un triage
a medias. Si la verificación falla, se agota el presupuesto o no hay tiempo, el
run termina igual con el resultado de OCR y los hallazgos quedan sin triage
(fail-open al humano).

### 2. Contrato del verificador

Se escribe una regla JSON (`verify-rule.json`) aplicable a `**/*` y un
contexto con los hallazgos del run. `ocr scan` no acepta `--background-file`,
así que el contexto viaja en línea con `--background`; la regla y el contexto
comparten estas instrucciones:

- Verificar solo los hallazgos listados; no reportar hallazgos nuevos.
- Un comentario por hallazgo, colocado en sus líneas.
- La primera línea del comentario debe ser exactamente
  `QA-VERIFY <finding_id> <CONFIRM|REFUTE|UNCERTAIN>`.
- Después, `EVIDENCE:` seguido de una línea de código copiada literalmente del
  archivo en el head, y una frase de razonamiento.
- `REFUTE` exige código que contradiga la afirmación (el guard existe, el test
  existe, la ruta ya valida, etc.). Sin esa evidencia, responder `UNCERTAIN`.
- No emitir sugerencias de estilo, nombres ni tests.

Invocación (en el mismo clon, antes de borrarlo):

```
ocr scan --repo <clone> --path <archivos citados> --rule verify-rule.json \
  --background "<contexto con los hallazgos>" --format json \
  --provider <perfil> --model <modelo> \
  --max-tokens <techo> --max-tokens-budget <presupuesto> --timeout <minutos>
```

`scan` no tiene `--effort`: la segunda pasada usa el esfuerzo por defecto del
perfil. No se añade una política de esfuerzo que la CLI no puede aplicar.

Las rutas se agrupan por archivo y se limitan por política. La salida se parsea
como `nativeReview` (mismos `comments`/`summary`/`session_id`); si el esquema de
`scan` difiere, se ajusta el parser y se fija con un fixture. Si `ocr scan` no
acepta `--max-tokens-budget` o `--timeout`, se omiten esas flags y el límite se
aplica solo con el contexto y el deadline del run; el spike lo confirma.

### 3. Veredictos y evidencia

Para cada hallazgo del run (`last_seen_run_id = run.ID`, `status = open`):

- **CONFIRM** → `triage_verdict = confirmed`, `status = open`.
- **REFUTE** → `triage_verdict = false_positive`, `status = withdrawn`, solo si
  severidad es `minor`/`nit`.
- **UNCERTAIN**, sin respuesta, respuesta duplicada o evidencia que no aparece
  literalmente en el head → se deja abierto y sin veredicto.

Reglas de seguridad:

- La evidencia se valida contra el archivo del head: si el texto citado no
  existe, el veredicto se degrada a `UNCERTAIN`. Esto frena un `REFUTE`
  alucinado o inyectado por el propio contenido del hallazgo.
- Nunca se descarta un `major`/`blocker` automáticamente. Un `REFUTE` sobre
  esos se registra como evidencia para el humano, pero el hallazgo sigue
  abierto y bloqueando.
- Un hallazgo con `triage_source = human` no se toca, ni siquiera si OCR vuelve
  a reportarlo.
- Un hallazgo ya auto-descartado no se reabre mientras sigan igual su código
  citado (`existing_code`) y su rango. El fingerprint actual es solo
  path + categoría + título normalizado (`Fingerprint(af, nil)`), así que por sí
  solo no detectaría que el código cambió; la comparación del código citado es
  la que obliga a re-evaluar. Si cambia, el descarte automático se revierte y el
  hallazgo vuelve a la verificación.

### 4. Persistencia

Migración aditiva:

- `findings.existing_code` (texto, nullable): el código citado por OCR, para el
  pre-check de obsolescencia y para validar evidencia.
- `findings.verification` (jsonb, nullable): `{verdict, evidence, model,
  session_id, verified_at}`.
- `findings.triage_source` (texto, default `''`): `human` o `verifier`. La
  migración hace backfill de `human` en las filas existentes con
  `triaged_by <> ''`, para que el verificador no pise decisiones previas del
  panel o de `/qa`.
- `review_runs.verify_session_id` (texto, nullable) y
  `review_runs.verify_usage` (jsonb, nullable): trazabilidad y coste de la
  segunda pasada.

`TriageFinding` gana una fuente y una guarda: un triage de `verifier` no
sobrescribe uno `human`; devuelve un sentinel (por ejemplo
`ErrFindingHumanTriaged`) y el verificador lo cuenta como omitido. La
auditoría sigue escribiéndose en `audit_logs` con la acción
`finding.auto_triaged`, igual que el triage humano actual.

La verificación se guarda para **todas** las decisiones, incluidas las de
`dry_run` y las `UNCERTAIN` que no cambian estado. Así el panel y la comparación
de la semana de `dry_run` muestran qué respondió el verificador y con qué
evidencia, sin depender de que se haya aplicado.

`upsertFinding` deja de reabrir filas con `triage_verdict <> ''` en un run
posterior, salvo que hayan cambiado el `existing_code` o el rango citado. Es un
cambio de comportamiento deliberado: un descarte (humano o automático) se
mantiene mientras el hallazgo apunte al mismo código. Se cubre con test,
incluido el caso "el código cambió y el descarte se revierte".

`ExistingCode` hoy se descarta en `nativeFindings`; pasa a `AgentFinding` y a la
columna nueva.

### 5. Política y configuración

Por equipo (`qa_policies`), con defaults globales:

- `auto_triage_mode`: `off` | `dry_run` | `apply`. Default `off`.
- `auto_withdraw`: bool. Default `false`; si está off, un `REFUTE` se registra
  pero no descarta.
- `verify_max_findings` y `verify_max_files`: topes por run. Default 50 y 20.
- `verify_token_share`: porcentaje del presupuesto del run reservado para la
  verificación. Default 20 %.
- `verify_token_max`: tope fijo en tokens cuando el run no tiene presupuesto
  (`review_token_budget = 0`). Default 100 000. Sin él, los equipos sin
  presupuesto nunca verificarían.
- `verify_time_share`: porcentaje del tiempo restante reservado. Default 15 %.

`dry_run` calcula y guarda veredictos, los muestra en el panel y en el resumen,
pero no cambia el estado ni descarta. Es el modo recomendado para el primer
despliegue.

### 6. Publicación

El comentario de resumen existente (`<!-- qa-control-center:summary -->`) se
extiende con una sección de verificación:

- **Validado (N):** hallazgos `confirmed`, con ruta:línea y una línea de
  resumen. Son los únicos que reciben comentario inline nuevo.
- **Necesita tu criterio (K):** hallazgos `UNCERTAIN` o sin verificación, con
  la evidencia disponible. No reciben inline; quedan listados para el humano.
- **Descartado automáticamente (M):** solo el conteo y un enlace al panel. El
  detalle y la evidencia viven en la auditoría, no en el PR.

Si la verificación está apagada, falló o se agotó el presupuesto, el comentario
mantiene el comportamiento actual (todos los hallazgos abiertos) y añade una
nota de que no hubo verificación, para que el humano no asuma que lo listado
está confirmado.

Los hallazgos descartados no se publican y no reciben inline. El check y el
estado del PR no cambian por un descarte `minor`/`nit`; un `REFUTE` sobre
`major`/`blocker` no altera el bloqueo.

### 7. Seguridad

- El contexto y la regla se escriben en un directorio temporal con permisos
  restrictivos y se borran al terminar.
- El texto de los hallazgos es salida de modelo: se pasa por el saneado
  existente (`@`, `<`, `>`, secretos) y se encierra como no confiable.
- La evidencia se compara con el archivo real; no se publica texto del
  verificador sin sanear.
- No se añade shell ni herramientas nuevas: la segunda pasada usa el toolset de
  OCR sobre el clon que ya existe.
- Un fallo del verificador nunca descarta ni confirma nada.

### 8. Coste y límites

- La segunda pasada solo toca archivos con hallazgos, no el PR entero.
- Se verifican primero `minor` y luego `nit`, y se recortan por
  `verify_max_findings`/`verify_max_files`.
- Los tokens de la verificación se suman a los del run y se muestran aparte en
  `verify_usage`; las métricas de consumo siguen siendo reales.
- Si el tiempo restante no alcanza para el mínimo, se omite la verificación y
  se registra `verification_skipped = insufficient_time`.

### 9. Idempotencia y concurrencia

- Antes de aplicar veredictos se relee el run y el PR: si el head cambió o el
  run fue superseded, no se aplica ni se publica nada.
- Si un humano triasea mientras corre la verificación, su decisión gana.
- Repetir la verificación con el mismo resultado es idempotente
  (`triaged_by = ocr-verify:<modelo>`).
- Un run posterior reutiliza los veredictos guardados para no repetir la
  llamada si el head y el código citado no cambiaron. El veredicto guardado
  incluye el `head_sha` con el que se calculó; si no coincide, se re-verifica.

## Pruebas

- Unitarias del parser de veredictos: válido, prefijo ausente, duplicado,
  evidencia que no coincide, severidad no descartable.
- Unitarias de política: `off`/`dry_run`/`apply`, `auto_withdraw` off, humano
  gana, `major` nunca se descarta.
- Store: guarda de `triage_source`, `upsertFinding` no reabre descartados,
  migración.
- Integración con el binario OCR falso: review + scan, veredictos mixtos,
  publicación curada, inline solo confirmados, tokens sumados.
- E2E: un PR con un falso positivo claro y un hallazgo real; se descarta uno,
  se confirma el otro, el comentario muestra ambos y el segundo recibe inline.

## Despliegue

1. Spike corto: confirmar que `ocr scan --format json` emite el esquema
   esperado y que el modelo respeta `QA-VERIFY`/`EVIDENCE` en 1.14.1.
2. Implementar con `auto_triage_mode = off` y tests.
3. Activar `dry_run` en el equipo `mitra` y comparar veredictos con el triage
   humano de una semana.
4. Activar `apply` con `auto_withdraw` solo para `minor`/`nit`.
5. Medir: confirmados/descartados/dudosos, tokens extra y cuántos descartes
   automáticos revierte un humano.

## No objetivos

- Auto-merge ni aprobación de PRs.
- Descartar `major`/`blocker` sin humano.
- Sustituir la revisión humana por completo.
- Cambiar el motor de revisión o el modelo de OCR.
- Corregir el código del PR.

## Decisiones tomadas (revisables)

- Verificar con `ocr scan` focalizado, no con una segunda revisión completa.
- Contrato de veredicto con prefijo legible por máquina y evidencia obligatoria.
- Confirmar cualquier severidad; descartar solo `minor`/`nit`.
- `dry_run` como primer modo; `off` por defecto.
- Un solo comentario de resumen curado; inline solo para confirmados.
- Conservar el descarte en runs posteriores hasta que cambie el fingerprint.
- Partir `CompleteReviewRun` para que la fase `verifying` sea visible y los
  tokens de la verificación cuenten en el run.

## Preguntas abiertas

- ¿`ocr scan` de 1.14.1 honra `--background-file` y la regla en tareas por
  archivo? Lo resuelve el spike.
- ¿El modelo respeta el prefijo `QA-VERIFY` de forma fiable? Si no, se cae a
  matching por ruta + categoría + solape de líneas y todo lo ambiguo queda
  `UNCERTAIN`.
- ¿Conviene un perfil LLM distinto para verificar? Se deja configurable; por
  defecto, el del equipo.
- ¿Se persisten las sesiones de OCR para usar `session compare` en el futuro?
  Fuera de alcance; la comparación se hace en proceso.
