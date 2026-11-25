# Roadmap de correcciones del motor de revisión

Actualizado: 2026-11-25. Este documento es la fuente de verdad del avance: cada PR que cierra un issue actualiza su fila y la fase correspondiente.

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
| #72 | Techo de 270k, caché y esfuerzo por equipo | M/L | #71 | **Hecho** (#106, #108, #109, #118, #120): el motor aplica techo, esfuerzo, tope y troceo por equipo en cada run; la medición con datos reales queda en la fase 6 |
| #80 | Diffs enormes por partes sin abandonar el PR | L | #71, #72 | **Hecho** (#118, #120): commits sintéticos por hunks completos, deduplicación de hallazgos de frontera y cobertura por archivo |
| #74 | Revisión incremental por push y antirrebote | L | #71 | **Hecho** (#113, #114): antirrebote de 2 min y revisión `<head revisado>..<head>` solo cuando la base no cambió y el head desciende del anterior; vuelta al diff completo en el resto de casos |
| #76 | Seguimiento del objetivo global | — | todas | **Cerrado** (2026-11-22): decisión registrada en el issue de no esperar la semana de muestra real; la medición con proveedor real vive ahora en la fase 6 y se apoya en el corpus de #140 |
| #140 | Corpus local y evaluación ciega por lenguaje y tipo | M | #78, #135, #136, #137 | **Hecho** (#180): corpus versionado de 15 PRs, arnés determinista y línea base medida (17 archivos revisables, 15 revisados, precisión 8/9, 1 grave omitido, 1 limpio marcado con defecto) |

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
- #72 parcial (#106, #108, #109): ajuste global `review_context_ceiling` (270k), techo por equipo, esfuerzo y tope de gasto configurables y aplicados por el motor nativo: techo efectivo como `--max-tokens` por grupo, esfuerzo de la política como `--effort`, tope opcional como `--max-tokens-budget` y corte por presupuesto como `inconclusive`/`token_budget`. Los runs nativos guardan `max_context_tokens` NULL porque OCR 1.14 no reporta esa muestra. El troceo de diffs enormes llegó en #80 (#118, #120); queda la medición con proveedor real, que se sigue en la fase 6. El techo es por grupo, no un tope total.
- #74 hecho (#113, #114): cada push revisa `<head revisado>..<head>` cuando la base y la ascendencia del head anterior lo permiten; el run persiste `base_sha`, los hallazgos fuera del rango revisado siguen abiertos y cualquier force-push, cambio de base o run previo incompleto vuelve al diff completo. El antirrebote de 2 min llegó en #113.
- #60 y #62 hechos (#115): con OCR nativo las revisiones ya no ejecutan OpenCode; `review_agent_steps` solo acota el chequeo de salud, que ahora recibe el valor configurado en vez de caer al valor por defecto.
- 2026-11-23: verificación automática de hallazgos: una segunda pasada `ocr scan` responde `CONFIRM`/`REFUTE`/`UNCERTAIN` con evidencia literal, la política del equipo decide si se aplica (nunca sobre `major`/`blocker` ni con la regla de descarte apagada) y el comentario del PR se publica curado. Queda rodarla en escalera (`off` → `dry_run` → `apply` → `auto_withdraw`) en la fase 6.
- 2026-11-24: revisados los 30 issues abiertos (#135–#164); este documento recogió su orden en el backlog de abajo.
- 2026-11-25: **el backlog 2026-11-24 queda completo**. #135–#163 cerrados con sus PRs en `main` (entre otros #174, #179, #180, #181–#188, #189–#196); solo #164 sigue abierto como seguimiento. El corpus y la línea base de #140 están medidos. Lo que queda es la fase 6: escalera de verificación, medición real de #65/#66 y promoción del dictamen arquitectónico por clase.

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
- [x] #65 Variante de razonamiento por perfil declarada a OpenCode (`reasoning` + entrada explícita en compatibles con OpenAI) y tokens de razonamiento por run en el panel (#96). Faltan las mediciones con proveedor real (reducción, p95 < 30 s, calidad del JSON), en la fase 6.
- [x] #66 Clave de caché `prompt_cache_key` estable por perfil en el cuerpo de cada petición, verificada con el banco de tokens (#97). Falta medir la tasa de acierto con el proveedor real, en la fase 6.

### Fase 4 — Motor nativo OCR

Objetivo: dejar de pagar un bucle genérico de OpenCode y usar los controles de coste de OCR.

- [x] #79 Evaluar OCR 1.14.0 (exclusiones y extensiones), subir la versión del worker y comparar cobertura antes/después. No bloquea #71. (#98) Cobertura del PR de referencia: 5 → 7 revisables; 4 archivos Rust de test/bench excluidos y 6 extensiones nuevas soportadas.
- [x] #71 Una sola invocación a `ocr review` por revisión: argumentos y entorno del perfil, mapeo de hallazgos y estados, clave fuera de disco, fixtures reales. (#105, #107)
- [x] #72 **Hecho** (#106, #108, #109, #118, #120): `review_context_ceiling` global y techo por equipo aplicados como `--max-tokens` por grupo, esfuerzo de la política como `--effort`, tope de gasto opcional como `--max-tokens-budget` (corte `inconclusive`/`token_budget`), caché `{ocr_session_key}` y NULL en `max_context_tokens` cuando OCR no da muestra. El troceo de diffs que no caben llegó con #80 (#118, #120); la medición con datos reales se hace en la fase 6. No es un tope del gasto total.
- [x] #80 Trocear diffs enormes por hunks completos y commits sintéticos, deduplicar hallazgos de trozos contiguos y publicar cobertura por archivo (`reviewed_by_parts`, `not_reviewed` con motivo); nunca abandonar el PR entero. (#118, #120)
- [x] #74 Antirrebote de 2 min (#113) y revisión incremental `<sha revisado>..<head>` con vuelta a completa ante force-push, cambio de base o run parcial (#114).

### Fase 5 — Cierre (histórica)

- [x] #76 Cerrado el 2026-11-22 con la decisión registrada en el issue: no se espera la semana de muestra real; la medición con proveedor real pasa al corpus de #140 y a la fase 6.
- [ ] Rodar la verificación automática en escalera y revisar los descartes que un humano restaure antes de ampliarla (fase 6).

## Backlog 2026-11-24 — revisión autónoma y dictamen arquitectónico (completo)

Objetivo: que la revisión concluya cuando tiene cobertura y evidencia suficientes, entregue todos los defectos confirmados en GitHub y derive a una persona solo cuando quede una incertidumbre material. #164 mantiene el seguimiento y sus casillas mandan; este documento fija el orden.

| Issue | Tema | Depende de | Fase | Estado |
|---|---|---|---|---|
| #155 | Regresiones locales del E2E: fixture de anclaje, timeout y `VerifyUsage` | — | 0 | **Hecho** |
| #141 | Invalidar la revisión cuando cambia la comparación base–head | — | 1 | **Hecho** |
| #144 | Identidad por PR y continuidad de cada hallazgo | — | 1 | **Hecho** |
| #135 | Cobertura real antes de declarar completa una revisión | — | 1 | **Hecho** |
| #136 | Evidencia verificable antes de publicar o bloquear | — | 1 | **Hecho** |
| #138 | Entrega durable e idempotente, sin resultados de SHAs obsoletos | #141 | 1 | **Hecho** |
| #142 | PRs sin job y bloqueo de cabeza de cola | #137, #138 | 1 | **Hecho** |
| #145 | Preservar las decisiones humanas al sincronizar hilos | #136, #137, #138, #144 | 1 | **Hecho** |
| #143 | Veredicto único con el CI requerido vigente | #135, #137, #138, #141 | 1 | **Hecho** |
| #137 | Solicitar revisión humana cuando el motor no concluye | #135, #136 | 2 | **Hecho** |
| #146 | Registrar el resultado humano de una revisión inconclusa | #135, #137, #141, #143, #145 | 2 | **Hecho** |
| #152 | Reevaluar objeciones desde los hilos del PR | #136, #137, #138, #145 | 2 | **Hecho** |
| #153 | Política efectiva por repositorio con herencia y auditoría | #137, #140, #141, #143, #150 | 2 | **Hecho** |
| #154 | Colas estancadas y alertas no entregadas | #138, #140, #142 | 2 | **Hecho** |
| #162 | Hallazgos de diseño multiarchivo o de PR, con una sola entrega | #136, #138, #144, #145, #157 | 2 | **Hecho** |
| #139 | Descripción del PR y contexto del repositorio | — | 3 | **Hecho** |
| #147 | Cobertura de texto y hunks grandes que OCR omite | #135, #136, #137, #140 | 3 | **Hecho** |
| #148 | PRs de forks con política de confianza y presupuesto | #137, #140, #142 | 3 | **Hecho** |
| #149 | Reanudar solo las partes pendientes | #135, #137, #138, #141 | 3 | **Hecho** |
| #150 | Perfil de respaldo autorizado y consumo atribuido | #137, #140, #149 | 3 | **Hecho** |
| #151 | Reserva efectiva del presupuesto de verificación | #136, #137, #149, #150 | 3 | **Hecho** |
| #156 | Plazo global con cierre gradual de OCR | #137, #149 | 3 | **Hecho** |
| #140 | Corpus local y evaluación ciega por lenguaje y tipo | #78, #135, #136, #137 | 0 (corpus) / 4 (medición) | **Hecho**: corpus, arnés y línea base |
| #157 | Dictamen arquitectónico por comparación, en observación | #135, #136, #137, #140, #141, #143, #146, #151, #153 | 4 | **Hecho** |
| #158 | Límites y decisiones de arquitectura contra línea base | #136, #139, #153, #157 | 4 | **Hecho** |
| #159 | Compatibilidad de contratos versionados base–head | #135, #136, #140, #147, #157 | 4 | **Hecho** |
| #160 | Impacto en consumidores de otros repositorios | #137, #139, #148, #153, #157, #159 | 4 | **Hecho** |
| #161 | Migraciones, despliegue y operación con evidencia del cambio | #136, #137, #140, #143, #157, #159 | 4 | **Hecho** |
| #163 | Revalidar la evidencia arquitectónica tras push parcial | #135, #136, #141, #144, #149, #157, #158, #162 | 4 | **Hecho** |

### Notas de la revisión

- **#137 contra #148**: resuelto con la política de #153. Un PR externo autorizado se analiza en modo lectura desde `refs/pull/<n>/head` del repositorio base, con límites por equipo y autor y datos privados desactivados por defecto; lo no autorizado o fuera de límites deriva a una persona con causa concreta. #137 sigue siendo la vía de derivación.
- **#151**: cerrado. La reserva de verificación se aparta antes de la pasada principal, se reparte por intento y tiene plazo propio; #156 añade el plazo global con cierre gradual.
- **#155**: no fue un fallo de producto; eran regresiones de los tests de #134 que no se ejecutaban en Windows. Corregido, con la suite completa en Linux como gate.
- **#136**: cerrado. La evidencia se valida contra el commit (SHA, ruta, rango y línea literal), también en resultados parciales y hallazgos con varias citas.
- **Modelo de publicación**: #138 (entrega durable por pasos), #143 (CI por veredicto), #146 (veredicto humano) y #162 (una sola superficie por hallazgo) comparten el mismo flujo; cualquier cambio futuro en publicación debe respetar los tres.
- **Dictamen arquitectónico**: #157–#163 están en **observación** por diseño. No cambian conclusión, estado de QA ni publicación inline; solo el resumen, el check y el panel. La promoción por clase es la fase 6.

## Fase 6 — Medición real y promoción

Objetivo: pasar de «implementado y en observación» a «medido y promovido por clase», sin ampliar la autonomía sin datos.

- [ ] Rodar la verificación automática en escalera (`off` → `dry_run` → `apply` → `auto_withdraw`) sobre PRs reales y revisar los descartes que un humano restaure. Nunca sobre `major`/`blocker` ni con la regla de descarte apagada.
- [ ] Medir #65 (razonamiento y latencia) y #66 (tasa de acierto de caché) con un proveedor real, usando el arnés de #140.
- [ ] Ampliar la muestra del corpus de #140 hasta el criterio de autonomía documentado (300 PRs con comentarios evaluados) y recalcular precisión, graves omitidos, citas y derivaciones por lenguaje y tipo.
- [ ] Promover el dictamen arquitectónico por clase a un piloto `COMMENT` con umbrales acordados. Medir falsos positivos, defectos graves omitidos y derivaciones injustificadas antes de evaluar su contribución al gate, con reversión prevista.
- [ ] Cerrar los huecos conocidos antes de promover: editor de consumidores en el panel (#160), contratos YAML/GraphQL como `unknown` (#159), plazo propio de análisis en OCR y citas multiarchivo estructuradas (#157, #162), y sonda de visibilidad real de repositorios (#160).

Criterio de cierre de #164: cada clase promovida tiene evidencia independiente suficiente, la escalera está rodada y los huecos conocidos están decididos (cerrados o aceptados por escrito).

## Orden de ejecución

1. Fase 1 completa (5 PRs pequeños, sin dependencias externas), hecha.
2. #69 y #68 (dos PRs de fiabilidad), hechos.
3. #73 y #77 (consumo y política), hechos.
4. #75, #65 y #66 (salud y latencia), hechos.
5. Motor nativo: #79, #71, #72, #74 y #80 hechos.
6. Cierre de #76, hecho.
7. Backlog 2026-11-24, fase 0: #155 deja la suite local reproducible en Linux, hecho.
8. Fase 1: #141, #144, #135, #136, #138, #142, #145 y #143, hechos.
9. Fases 2–4 del backlog, hechas; #140 con corpus y línea base medidos.
10. Fase 6: escalera de verificación y medición real de #65/#66.
11. Fase 6: ampliar el corpus al criterio de autonomía y promover por clase.
12. Cierre de #164.

## Reglas de avance

- Un issue por rama y PR, con `Closes #N` en el cuerpo.
- Gate local antes de cada PR: `go test ./...` completo en un contenedor `golang:1.27` sobre el daemon `kiri` y, cuando aplique, el banco de tokens en `qa-control-center-worker:local`. Nada de CI.
- Cada PR actualiza la fila del issue y marca su casilla en la fase.
- Si un issue cambia de alcance, se anota aquí antes de implementarlo.
