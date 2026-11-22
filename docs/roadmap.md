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
| #59 | Agente solo-diff: prompt dedicado, sin exploración | S | #70 | **Parcial**: herramientas denegadas y 8 pasos; falta el prompt dedicado |
| #60 | No reenviar el historial completo en cada paso | L | #71 | **Parcial**: menos pasos y menos herramientas; el reenvío sigue siendo intrínseco a OpenCode |
| #62 | Presupuesto de pasos por run, no por pasada | S | — | **Parcial**: 8 pasos por pasada; falta el tope por run |
| #63 | Sumar el consumo de todos los pasos y pasadas | M | — | Pendiente |
| #61 | Planificar pasadas por bytes (límite de 50 KB de OpenCode) | S/M | — | Pendiente |
| #67 | Aclarar que `review_agent_steps` es por pasada | XS | — | Pendiente |
| #69 | Drenar revisiones en vuelo al recrear el worker | M | — | **Hecho** (#91) |
| #68 | Refrescar el token de clonado de runs en cola | S/M | — | Pendiente |
| #73 | Registrar y mostrar el consumo real (columnas, API, panel) | M/L | #63 | Pendiente |
| #77 | Aplicar `exclude_globs` y `extra_instructions` de la política | M | — | Pendiente |
| #75 | Chequeos de salud sin coste de agente (una llamada, intervalo 6 h) | S/M | — | Pendiente |
| #65 | Controlar tokens de razonamiento y latencia | S/M | — | Pendiente |
| #66 | Verificar y activar caché de prompt | S | #71 | Pendiente |
| #79 | Evaluar y subir OCR 1.14.0 | S | — | Pendiente |
| #71 | Usar `ocr review` nativo como motor | L | #79 (recomendado) | Pendiente |
| #72 | Techo de 270k, caché y esfuerzo por equipo | M/L | #71 | Pendiente |
| #80 | Diffs enormes por partes sin abandonar el PR | L | #71, #72 | Pendiente |
| #74 | Revisión incremental por push y antirrebote | L | #71 | Pendiente |
| #76 | Seguimiento del objetivo global | — | todas | **Seguimiento** |

## Fases

### Fase 1 — Higiene del bucle (en curso)

Objetivo: exprimir lo que ya está en `main` y cerrar los huecos baratos del agente actual mientras se prepara la migración.

- [ ] #67 Aclarar la etiqueta y la ayuda de `review_agent_steps` («Pasos por pasada») y su relación con `max_passes`.
- [ ] #59 Añadir `agent.qa-reviewer.prompt`: el diff completo va adjunto, no explorar el repo, responder solo el JSON. Verificar ≤4 pasos sin `grep`/`glob`.
- [ ] #61 Planificar pasadas por bytes (~45 KB) además de por líneas y cubrir el caso >50 KB con test.
- [ ] #62 Aplicar el presupuesto de pasos por run, no solo por pasada, y alinear la etiqueta del panel.
- [ ] #63 Sumar todos los eventos de uso de todos los pasos y pasadas; test con varios `step_finish`.

### Fase 2 — Fiabilidad de la cola

Objetivo: que un deploy o una cola lenta no pierda revisiones ni gaste tokens sin publicar.

- [x] #69 Drenar el worker en el apagado: dejar de reclamar, esperar un margen acotado y, si hay que interrumpir, marcar el run terminal y reencolarlo al momento.
- [ ] #68 Refrescar el token de clonado al reclamar el run (o guardar la caducidad y renovarlo antes del clone), con reintento y error accionable.

### Fase 3 — Visibilidad, política y salud

Objetivo: ver dónde se gasta y que la política del equipo se aplique de verdad.

- [ ] #73 Columnas estructuradas (`input/output/cache_read/cache_write/total/max_context`), migración, API y vistas de PR, lead y perfiles. Los runs antiguos muestran «sin datos».
- [ ] #77 Pasar `exclude_globs` a OCR y `extra_instructions` al contexto saneado; editarlos en la vista de Agentes.
- [ ] #75 Sustituir el agente del chequeo de salud por una llamada mínima sin herramientas; intervalo por defecto de 6 h; solo perfiles activos y en uso.
- [ ] #65 Medir y bajar los tokens de razonamiento: modelo sin razonamiento o variante de bajo esfuerzo, con la calidad del JSON intacta.
- [ ] #66 Verificar la caché de prompt en CommandCode y documentar el resultado; si no la soporta, apoyarse en el diseño de una petición por pasada.

### Fase 4 — Motor nativo OCR

Objetivo: dejar de pagar un bucle genérico de OpenCode y usar los controles de coste de OCR.

- [ ] #79 Evaluar OCR 1.14.0 (exclusiones y extensiones), subir la versión del worker y comparar cobertura antes/después. No bloquea #71.
- [ ] #71 Una sola invocación a `ocr review` por revisión: argumentos y entorno del perfil, mapeo de hallazgos y estados, clave fuera de disco, fixtures reales.
- [ ] #72 Ajuste `review_context_ceiling` (270k), caché por conversación y esfuerzo por equipo; tope de gasto opcional desactivado por defecto.
- [ ] #80 Trocear diffs enormes por hunks, deduplicar hallazgos de trozos contiguos y publicar cobertura por archivo; nunca abandonar el PR entero.
- [ ] #74 Revisión incremental `<sha revisado>..<head>` con vuelta a completa ante force-push, cambio de base o run parcial; antirrebote de 2 min.

### Fase 5 — Cierre

- [ ] #76 Cerrar el seguimiento cuando una semana de PRs reales cumpla el criterio de cierre.

## Orden de ejecución

1. Fase 1 completa (5 PRs pequeños, sin dependencias externas).
2. #69 y #68 (dos PRs de fiabilidad).
3. #73 y #77 (consumo y política).
4. #75, #65 y #66 (salud y latencia).
5. #79 → #71 → #72 → #80 → #74 (motor nativo).
6. Cierre de #76.

## Reglas de avance

- Un issue por rama y PR, con `Closes #N` en el cuerpo.
- Gate local antes de cada PR: `go test ./...` completo en un contenedor `golang:1.27` sobre el daemon `kiri` y, cuando aplique, el banco de tokens en `qa-control-center-worker:local`. Nada de CI.
- Cada PR actualiza la fila del issue y marca su casilla en la fase.
- Si un issue cambia de alcance, se anota aquí antes de implementarlo.
