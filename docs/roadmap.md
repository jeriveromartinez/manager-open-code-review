# Roadmap de correcciones del motor de revisión

Actualizado: 2026-11-24. Este documento es la fuente de verdad del avance: cada PR que cierra un issue actualiza su fila y la fase correspondiente.

## Objetivo

Que una revisión real, con los valores por defecto, cumpla a la vez:

- ninguna conversación con el modelo supera **270k tokens de contexto**;
- los PRs con diffs enormes se revisan **enteros o por partes**, nunca se abandonan;
- un deploy o un push no pierde ni repite trabajo innecesario;
- el **consumo real** (tokens, caché y contexto máximo) es visible por revisión, equipo y perfil;
- la política del equipo (`exclude_globs`, `extra_instructions`, esfuerzo y techo) se aplica de verdad.

Criterio de cierre de #76: una semana de PRs reales sin ningún run por encima de 270k de contexto, con los diffs enormes revisados y el consumo visible en el panel.

## Estado actual

| Issue | Tema | Tamaño | Depende de | Estado |
|---|---|---|---|---|
| #70 | Recortar el bucle de OpenCode (solo `read`, 8 pasos, sin título, sin ruta del diff, reintento con `--continue`) | M | — | **Hecho** (#81) |
| #78 | Banco de consumo con proveedor falso | M | #70 | **Hecho** (#83) |
| #64 | Suprimir la generación automática de título | XS | — | **Hecho** (#81) |
| #84 | Plantilla de PR con `Closes #` | XS | — | **Hecho** (#84) |
| #59 | Agente solo-diff: prompt dedicado, sin exploración | S | #70 | **Hecho** (#87) |
| #60 | No reenviar el historial completo en cada paso | L | #71 | **Hecho** (#115): el motor nativo de OCR sustituye el bucle de OpenCode, así que el reenvío del historial deja de aplicar a las revisiones |
| #62 | Presupuesto de pasos por run, no por pasada | S | #71 | **Hecho** (#115): `review_agent_steps` ya no limita revisiones (OCR nativo), solo el número de pasos del chequeo de salud, que ahora honra el valor configurado |
| #63 | Sumar el consumo de todos los pasos y pasadas | M | — | **Hecho** (#89) |
| #61 | Planificar pasadas por bytes (límite de 50 KB de OpenCode) | S/M | — | **Hecho** (#88) |
| #67 | Aclarar que `review_agent_steps` es por pasada | XS | — | **Hecho** (#86) |
| #69 | Drenar revisiones en vuelo al recrear el worker | M | — | **Hecho** (#91) |
| #68 | Refrescar el token de clonado de runs en cola | S/M | — | **Hecho** (#92) |
| #73 | Registrar y mostrar el consumo real (columnas, API, panel) | M/L | #63 | **Hecho** (#93, #101): columnas, detalle de PR, tile de lead y consumo por perfil con chequeos de salud |
| #77 | Aplicar `exclude_globs` y `extra_instructions` de la política | M | — | **Hecho** (#94) |
| #75 | Chequeos de salud sin coste de agente (una llamada, intervalo 6 h) | S/M | — | **Hecho** (#95) |
| #65 | Controlar tokens de razonamiento y latencia | S/M | — | **Hecho** (#96); faltan las mediciones con proveedor real |
| #66 | Verificar y activar caché de prompt | S | #71 | **Hecho** (#97); falta medir la tasa de acierto con el proveedor real |
| #79 | Evaluar y subir OCR 1.14.0 | S | — | **Hecho** (#98) |
| #71 | Usar `ocr review` nativo como motor | L | #79 (recomendado) | **Hecho** (#105, #107): una invocación nativa por run y fixtures reales de caché y timeout |
| #72 | Techo de 270k, caché y esfuerzo por equipo | M/L | #71 | **Hecho** (#106, #108, #109, #118, #120): el motor aplica techo, esfuerzo, tope y troceo por equipo en cada run; la medición con datos reales queda en #76 |
| #80 | Diffs enormes por partes sin abandonar el PR | L | #71, #72 | **Hecho** (#118, #120): commits sintéticos por hunks completos, deduplicación de hallazgos de frontera y cobertura por archivo |
| #74 | Revisión incremental por push y antirrebote | L | #71 | **Hecho** (#113, #114): antirrebote de 2 min y revisión `<head revisado>..<head>` solo cuando la base no cambió y el head desciende del anterior; vuelta al diff completo en el resto de casos |
| #76 | Seguimiento del objetivo global | — | todas | **Cerrado** (2026-11-22): decisión registrada en el issue de no esperar la semana de muestra real; la medición con proveedor real se traslada al corpus de #140 |

## Progreso

- 2026-11-21: revisados los 21 issues abiertos; #70, #78 y #64 cerrados con evidencia en `main`.
- 2026-11-22: con #118 y #120, #80 y #72 quedan cerrados en código; solo #76 (seguimiento) sigue abierto. #60, #62, #70, #71, #73, #74, #75, #77, #78 y #79 también están cerrados con sus PRs en `main`.
- En `main`: roadmap #85 y Fase 1 completa (#86, #87, #88, #89).
- Fase 2 completa: #69 (#91) y #68 (#92). Siguiente: Fase 3, #73 y #77.
- #73 hecho (#93, #101): consumo real por ejecución, detalle de PR, tile de lead, consumo por perfil y chequeos de salud en `llm_health_checks`.
- #77 hecho (#94): exclusión de archivos y instrucciones del equipo aplicadas de verdad.
- #75 hecho (#95): chequeo de salud con una petición mínima sin herramientas, intervalo de 6 h y solo perfiles en uso. Siguiente: #65 y #66.
- #65 hecho (#96): la variante del perfil deja de ignorarse y cada run muestra sus tokens de razonamiento.
- #66 hecho (#97): cada petición lleva `prompt_cache_key` estable por perfil y el banco de tokens verifica que los `cached_tokens` llegan al consumo del run. Faltan las mediciones con el proveedor real.
- #79 hecho (#98): OCR 1.14.0 fijado en el worker; en el PR de referencia la cobertura pasa de 5 a 7 archivos revisables (4 archivos Rust de test/bench excluidos, 6 extensiones nuevas). Siguiente: #71.
- #71 hecho (#105, #107): el worker usa una sola invocación a `ocr review` por revisión, con argumentos y entorno del perfil y mapeo de hallazgos/estados; los fixtures reales cubren caché y timeout de OCR 1.14.
- #72 parcial (#106, #108, #109): ajuste global `review_context_ceiling` (270k), techo por equipo, esfuerzo y tope de gasto configurables y aplicados por el motor nativo: techo efectivo como `--max-tokens` por grupo, esfuerzo de la política como `--effort`, tope opcional como `--max-tokens-budget` y corte por presupuesto como `inconclusive`/`token_budget`. Los runs nativos guardan `max_context_tokens` NULL porque OCR 1.14 no reporta esa muestra. El troceo de diffs enormes llegó en #80 (#118, #120); queda la medición con proveedor real, que se sigue en #76. El techo es por grupo, no un tope total.
- #74 hecho (#113, #114): cada push revisa `<head revisado>..<head>` cuando la base y la ascendencia del head anterior lo permiten; el run persiste `base_sha`, los hallazgos fuera del rango revisado siguen abiertos y cualquier force-push, cambio de base o run previo incompleto vuelve al diff completo. El antirrebote de 2 min llegó en #113.
- #60 y #62 hechos (#115): con OCR nativo las revisiones ya no ejecutan OpenCode; `review_agent_steps` solo acota el chequeo de salud, que ahora recibe el valor configurado en vez de caer al valor por defecto.
- 2026-11-23: verificación automática de hallazgos en `feat/ocr-auto-triage`: una segunda pasada `ocr scan` responde `CONFIRM`/`REFUTE`/`UNCERTAIN` con evidencia literal, la política del equipo decide si se aplica (nunca sobre `major`/`blocker` ni con la regla de descarte apagada) y el comentario del PR se publica curado. Queda rodarla en escalera (`off` → `dry_run` → `apply` → `auto_withdraw`) y medir con PRs reales dentro de #76.
- 2026-11-24: revisados los 30 issues abiertos (#135–#164) sobre `182168f`. #164 mantiene el seguimiento del nuevo programa y este documento recoge su orden en el backlog de abajo. Primer cambio: #155; después #141 y #144 en paralelo.

## Fases

### Fase 1 — Higiene del bucle (completa)

Objetivo: exprimir lo que ya está en `main` y cerrar los huecos baratos del agente actual mientras se prepara la migración.

- [x] #67 Aclarar la etiqueta y la ayuda de `review_agent_steps` («Pasos por pasada») y su relación con `max_passes`. (#86)
- [x] #59 Añadir `agent.qa-reviewer.prompt`: el diff completo va adjunto, no explorar el repo, responder solo el JSON. (#87)
- [x] #61 Planificar pasadas por bytes (~45 KB) además de por líneas y cubrir el caso >50 KB con test. (#88)
- [x] #62 Aplicar el presupuesto de pasos por run, no solo por pasada, y alinear la etiqueta del panel. (#115) Con OCR nativo las revisiones ya no usan OpenCode; `review_agent_steps` solo acota el chequeo de salud y ahora honra el valor configurado.
- [x] #63 Sumar todos los eventos de uso de todos los pasos y pasadas; test con varios `step_finish`. (#89)

### Fase 2 — Fiabilidad de la cola

Objetivo: que un deploy o una cola lenta no pierda revisiones ni gaste tokens sin publicar.

- [x] #69 Drenar el worker en el apagado: dejar de reclamar, esperar un margen acotado y, si hay que interrumpir, marcar el run terminal y reencolarlo al momento.
- [x] #68 Refrescar el token de clonado al reclamar el run (o guardar la caducidad y renovarlo antes del clone), con reintento y error accionable.

### Fase 3 — Visibilidad, política y salud

Objetivo: ver dónde se gasta y que la política del equipo se aplique de verdad.

- [x] #73 Columnas estructuradas (`input/output/cache_read/cache_write/total/max_context`), migración, API y vistas de PR y lead (#93), más consumo por perfil y chequeos de salud en `llm_health_checks` con `GET /api/llm-profiles/usage` y la columna de consumo en Agentes LLM (#101). Los runs antiguos muestran «sin datos».
- [x] #77 Pasar `exclude_globs` a OCR y `extra_instructions` al contexto saneado; editarlos en la vista de Agentes (#94).
- [x] #75 Sustituir el agente del chequeo de salud por una llamada mínima sin herramientas; intervalo por defecto de 6 h; solo perfiles activos y en uso. (#95)
- [x] #65 Variante de razonamiento por perfil declarada a OpenCode (`reasoning` + entrada explícita en compatibles con OpenAI) y tokens de razonamiento por run en el panel (#96). Faltan las mediciones con proveedor real (reducción, p95 < 30 s, calidad del JSON).
- [x] #66 Clave de caché `prompt_cache_key` estable por perfil en el cuerpo de cada petición, verificada con el banco de tokens (#97). Falta medir la tasa de acierto con el proveedor real; si no la honra, se mantiene el diseño de una petición por pasada.

### Fase 4 — Motor nativo OCR

Objetivo: dejar de pagar un bucle genérico de OpenCode y usar los controles de coste de OCR.

- [x] #79 Evaluar OCR 1.14.0 (exclusiones y extensiones), subir la versión del worker y comparar cobertura antes/después. No bloquea #71. (#98) Cobertura del PR de referencia: 5 → 7 revisables; 4 archivos Rust de test/bench excluidos y 6 extensiones nuevas soportadas.
- [x] #71 Una sola invocación a `ocr review` por revisión: argumentos y entorno del perfil, mapeo de hallazgos y estados, clave fuera de disco, fixtures reales. (#105, #107)
- [x] #72 **Hecho** (#106, #108, #109, #118, #120): `review_context_ceiling` global y techo por equipo aplicados como `--max-tokens` por grupo, esfuerzo de la política como `--effort`, tope de gasto opcional como `--max-tokens-budget` (corte `inconclusive`/`token_budget`), caché `{ocr_session_key}` y NULL en `max_context_tokens` cuando OCR no da muestra. El troceo de diffs que no caben llegó con #80 (#118, #120); la medición con datos reales se sigue en #76. No es un tope del gasto total.
- [x] #80 Trocear diffs enormes por hunks completos y commits sintéticos, deduplicar hallazgos de trozos contiguos y publicar cobertura por archivo (`reviewed_by_parts`, `not_reviewed` con motivo); nunca abandonar el PR entero. (#118, #120)
- [x] #74 Antirrebote de 2 min (#113) y revisión incremental `<sha revisado>..<head>` con vuelta a completa ante force-push, cambio de base o run parcial (#114).

### Fase 5 — Cierre (histórica)

- [x] #76 Cerrado el 2026-11-22 con la decisión registrada en el issue: no se espera la semana de muestra real; la medición con proveedor real pasa al corpus de #140.
- [ ] Rodar la verificación automática en escalera y revisar los descartes que un humano restaure antes de ampliarla.

## Backlog 2026-11-24 — revisión autónoma y dictamen arquitectónico

Objetivo: que la revisión concluya cuando tiene cobertura y evidencia suficientes, entregue todos los defectos confirmados en GitHub y derive a una persona solo cuando quede una incertidumbre material. #164 mantiene el seguimiento y sus casillas mandan; este documento fija el orden.

| Issue | Tema | Depende de | Fase |
|---|---|---|---|
| #155 | Regresiones locales del E2E: fixture de anclaje, timeout y `VerifyUsage` | — | 0 |
| #141 | Invalidar la revisión cuando cambia la comparación base–head | — | 1 |
| #144 | Identidad por PR y continuidad de cada hallazgo | — | 1 |
| #135 | Cobertura real antes de declarar completa una revisión | — | 1 |
| #136 | Evidencia verificable antes de publicar o bloquear | — | 1 |
| #138 | Entrega durable e idempotente, sin resultados de SHAs obsoletos | #141 | 1 |
| #142 | PRs sin job y bloqueo de cabeza de cola | #137, #138 | 1 |
| #145 | Preservar las decisiones humanas al sincronizar hilos | #136, #137, #138, #144 | 1 |
| #143 | Veredicto único con el CI requerido vigente | #135, #137, #138, #141 | 1 |
| #137 | Solicitar revisión humana cuando el motor no concluye | #135, #136 | 2 |
| #146 | Registrar el resultado humano de una revisión inconclusa | #135, #137, #141, #143, #145 | 2 |
| #152 | Reevaluar objeciones desde los hilos del PR | #136, #137, #138, #145 | 2 |
| #153 | Política efectiva por repositorio con herencia y auditoría | #137, #140, #141, #143, #150 | 2 |
| #154 | Colas estancadas y alertas no entregadas | #138, #140, #142 | 2 |
| #162 | Hallazgos de diseño multiarchivo o de PR, con una sola entrega | #136, #138, #144, #145, #157 | 2 |
| #139 | Descripción del PR y contexto del repositorio | — | 3 |
| #147 | Cobertura de texto y hunks grandes que OCR omite | #135, #136, #137, #140 | 3 |
| #148 | PRs de forks con política de confianza y presupuesto | #137, #140, #142 | 3 |
| #149 | Reanudar solo las partes pendientes | #135, #137, #138, #141 | 3 |
| #150 | Perfil de respaldo autorizado y consumo atribuido | #137, #140, #149 | 3 |
| #151 | Reserva efectiva del presupuesto de verificación | #136, #137, #149, #150 | 3 |
| #156 | Plazo global con cierre gradual de OCR | #137, #149 | 3 |
| #140 | Corpus local y evaluación ciega por lenguaje y tipo | #78, #135, #136, #137 | 0 (corpus) / 4 (medición) |
| #157 | Dictamen arquitectónico por comparación, en observación | #135, #136, #137, #140, #141, #143, #146, #151, #153 | 4 |
| #158 | Límites y decisiones de arquitectura contra línea base | #136, #139, #153, #157 | 4 |
| #159 | Compatibilidad de contratos versionados base–head | #135, #136, #140, #147, #157 | 4 |
| #160 | Impacto en consumidores de otros repositorios | #137, #139, #148, #153, #157, #159 | 4 |
| #161 | Migraciones, despliegue y operación con evidencia del cambio | #136, #137, #140, #143, #157, #159 | 4 |
| #163 | Revalidar la evidencia arquitectónica tras push parcial | #135, #136, #141, #144, #149, #157, #158, #162 | 4 |

### Notas de la revisión

- **#137 contra #148**: #137 deriva los forks a una revisión humana y #148 los analiza dentro del motor con límites. Hay que decidir cuál manda y bajo qué política antes de implementar la fase 3.
- **Trabajo previo parcial**: #142 ya repara runs en cola huérfanos en `trigger.go:196`, pero el barrido sigue excluyendo los runs en cola; #143 reevalúa el CI por entrega de check, pero la publicación aún puede declarar éxito sin CI; #145 audita los cambios de hilo sin autorizar; #135 mapea `complete` a `succeeded` aunque queden archivos excluidos.
- **#151** sigue abierto: #134 acota la verificación a lo que queda del presupuesto, pero la cuota no se reserva antes de la pasada principal y el verificador no tiene plazo propio.
- **#155** no es un fallo de producto: son regresiones de los tests de #134 que no se ejecutaban en Windows.
- **#136** está parcialmente cubierto por #134 (la evidencia se compara con el archivo citado), pero falta validar SHA, ruta, rango y cita contra el commit, y la evidencia del impacto.

## Orden de ejecución

1. Fase 1 completa (5 PRs pequeños, sin dependencias externas), hecha.
2. #69 y #68 (dos PRs de fiabilidad), hechos.
3. #73 y #77 (consumo y política), hechos.
4. #75, #65 y #66 (salud y latencia), hechos.
5. Motor nativo: #79, #71, #72, #74 y #80 hechos; solo queda la medición real en #76.
6. Cierre de #76.
7. Backlog 2026-11-24, fase 0: #155 deja la suite local reproducible en Linux.
8. Fase 1: #141 y #144 en paralelo; acordados sus contratos, #135 y #136; después #138, #142, #145 y #143. El corpus de #140 arranca en paralelo.
9. Fases 2–4 en el orden del backlog; el dictamen arquitectónico solo se promueve tras #140 y #162 verificados.

## Reglas de avance

- Un issue por rama y PR, con `Closes #N` en el cuerpo.
- Gate local antes de cada PR: `go test ./...` completo en un contenedor `golang:1.27` sobre el daemon `kiri` y, cuando aplique, el banco de tokens en `qa-control-center-worker:local`. Nada de CI.
- Cada PR actualiza la fila del issue y marca su casilla en la fase.
- Si un issue cambia de alcance, se anota aquí antes de implementarlo.
