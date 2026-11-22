# Roadmap de correcciones del motor de revisión

Actualizado: 2026-11-21. Este documento es la fuente de verdad del avance: cada PR que cierra un issue actualiza su fila y la fase correspondiente.

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
| #60 | No reenviar el historial completo en cada paso | L | #71 | **Parcial**: menos pasos y menos herramientas; el reenvío sigue siendo intrínseco a OpenCode |
| #62 | Presupuesto de pasos por run, no por pasada | S | #71 | **Parcial**: 8 pasos por pasada (máximo 32 por revisión); el tope por run se reevalúa con #71 |
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
| #71 | Usar `ocr review` nativo como motor | L | #79 (recomendado) | **Hecho** (#105) |
| #72 | Techo de 270k, caché y esfuerzo por equipo | M/L | #71 | **Parcial** (#106, #109): el motor aplica techo, esfuerzo y tope por equipo en cada run; faltan el troceo de #80 y la medición con datos reales |
| #80 | Diffs enormes por partes sin abandonar el PR | L | #71, #72 | Pendiente |
| #74 | Revisión incremental por push y antirrebote | L | #71 | Pendiente |
| #76 | Seguimiento del objetivo global | — | todas | **Seguimiento** |

## Progreso

- 2026-11-21: revisados los 21 issues abiertos; #70, #78 y #64 cerrados con evidencia en `main`.
- En `main`: roadmap #85 y Fase 1 completa (#86, #87, #88, #89).
- Fase 2 completa: #69 (#91) y #68 (#92). Siguiente: Fase 3, #73 y #77.
- #73 hecho (#93, #101): consumo real por ejecución, detalle de PR, tile de lead, consumo por perfil y chequeos de salud en `llm_health_checks`.
- #77 hecho (#94): exclusión de archivos y instrucciones del equipo aplicadas de verdad.
- #75 hecho (#95): chequeo de salud con una petición mínima sin herramientas, intervalo de 6 h y solo perfiles en uso. Siguiente: #65 y #66.
- #65 hecho (#96): la variante del perfil deja de ignorarse y cada run muestra sus tokens de razonamiento.
- #66 hecho (#97): cada petición lleva `prompt_cache_key` estable por perfil y el banco de tokens verifica que los `cached_tokens` llegan al consumo del run. Faltan las mediciones con el proveedor real.
- #79 hecho (#98): OCR 1.14.0 fijado en el worker; en el PR de referencia la cobertura pasa de 5 a 7 archivos revisables (4 archivos Rust de test/bench excluidos, 6 extensiones nuevas). Siguiente: #71.
- #71 hecho (#105): el worker usa una sola invocación a `ocr review` por revisión, con argumentos y entorno del perfil, mapeo de hallazgos/estados y fixtures reales.
- #72 parcial (#106, #109): ajuste global `review_context_ceiling` (270k), techo por equipo, esfuerzo y tope de gasto configurables y aplicados por el motor nativo: techo efectivo como `--max-tokens` por grupo, esfuerzo de la política como `--effort`, tope opcional como `--max-tokens-budget` y corte por presupuesto como `inconclusive`/`token_budget`. Los runs nativos guardan `max_context_tokens` NULL porque OCR 1.14 no reporta esa muestra. Quedan #80 (trocear diffs enormes) y la medición con proveedor real; el techo es por grupo, no un tope total.

## Fases

### Fase 1 — Higiene del bucle (en curso)

Objetivo: exprimir lo que ya está en `main` y cerrar los huecos baratos del agente actual mientras se prepara la migración.

- [x] #67 Aclarar la etiqueta y la ayuda de `review_agent_steps` («Pasos por pasada») y su relación con `max_passes`. (#86)
- [x] #59 Añadir `agent.qa-reviewer.prompt`: el diff completo va adjunto, no explorar el repo, responder solo el JSON. (#87)
- [x] #61 Planificar pasadas por bytes (~45 KB) además de por líneas y cubrir el caso >50 KB con test. (#88)
- [ ] #62 Aplicar el presupuesto de pasos por run, no solo por pasada, y alinear la etiqueta del panel. Se reevalúa junto con #71; mientras tanto el tope de 8/pasada ya acota el máximo a 32.
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
- [x] #71 Una sola invocación a `ocr review` por revisión: argumentos y entorno del perfil, mapeo de hallazgos y estados, clave fuera de disco, fixtures reales. (#105)
- [ ] #72 **Parcial** (#109): `review_context_ceiling` global y techo por equipo aplicados como `--max-tokens` por grupo, esfuerzo de la política como `--effort`, tope de gasto opcional como `--max-tokens-budget` (corte `inconclusive`/`token_budget`), caché `{ocr_session_key}` y NULL en `max_context_tokens` cuando OCR no da muestra. Falta #80 para trocear diffs que no caben y la medición con datos reales; no es un tope del gasto total.
- [ ] #80 Trocear diffs enormes por hunks, deduplicar hallazgos de trozos contiguos y publicar cobertura por archivo; nunca abandonar el PR entero.
- [ ] #74 Revisión incremental `<sha revisado>..<head>` con vuelta a completa ante force-push, cambio de base o run parcial; antirrebote de 2 min.

### Fase 5 — Cierre

- [ ] #76 Cerrar el seguimiento cuando una semana de PRs reales cumpla el criterio de cierre.

## Orden de ejecución

1. Fase 1 completa (5 PRs pequeños, sin dependencias externas).
2. #69 y #68 (dos PRs de fiabilidad).
3. #73 y #77 (consumo y política), hechos.
4. #75, #65 y #66 (salud y latencia).
5. #79 → #71 → #72 → #80 → #74 (motor nativo).
6. Cierre de #76.

## Reglas de avance

- Un issue por rama y PR, con `Closes #N` en el cuerpo.
- Gate local antes de cada PR: `go test ./...` completo en un contenedor `golang:1.27` sobre el daemon `kiri` y, cuando aplique, el banco de tokens en `qa-control-center-worker:local`. Nada de CI.
- Cada PR actualiza la fila del issue y marca su casilla en la fase.
- Si un issue cambia de alcance, se anota aquí antes de implementarlo.
