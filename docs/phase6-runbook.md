# Fase 6 — medición real y promoción del dictamen arquitectónico

Procedimiento para lo que queda del programa de revisión autónoma. El estado y
el orden están en [`docs/roadmap.md`](roadmap.md); este documento es la guía de
ejecución de la fase 6 y la referencia del issue #198.

## Qué está hecho y en observación

- Verificación automática de hallazgos: `auto_triage_mode` (`off`, `dry_run`,
  `apply`) y `auto_withdraw`, con reserva de presupuesto y validación de
  evidencia.
- Dictamen arquitectónico en **observación**: análisis OCR, línea base de
  decisiones, contratos versionados (JSON, YAML, proto, Go), consumidores con
  visibilidad real, evidencia operativa, citas estructuradas, plazo propio y
  revalidación tras pushes.
- Corpus de evaluación con 15 PRs y una línea base medida (#140).

Nada de esto cambia la conclusión del check, el estado de QA ni los comentarios
inline: solo el resumen, el texto del check y el panel. La promoción es lo que
está pendiente.

## 1. Escalera de verificación

La política es por equipo (y por repositorio como override) en **Agentes LLM →
política** o por API (`PUT /api/teams/{id}/policy`).

1. **`off`** — línea de base sin verificación automática.
2. **`dry_run`** — la segunda pasada y el dictamen se calculan y registran; el
   resultado no se aplica. Comparar, por PR, el veredicto automático con el
   humano en el panel y anotar:
   - confirmaciones correctas y refutaciones correctas;
   - hallazgos que un humano restauró tras un descarte automático;
   - llamadas `UNCERTAIN` (no bloquean nada).
3. **`apply`** — se aplican las decisiones de verificación. `auto_withdraw`
   sigue apagado: se retiran comentarios que la verificación refuta, pero no se
   cierra nada que un humano haya restaurado. Revisar esa lista antes de seguir.
4. **`auto_withdraw`** — con `apply`, permite retirar también con el descarte
   automático. Nunca se retira `major`/`blocker` y nunca se descarta sin
   evidencia literal vigente.

Criterio para avanzar de escalón: cero descartes automáticos que un humano
tenga que restaurar en la muestra, y ningún `major`/`blocker` retirado.
Registrar en el issue #198 el intervalo, el número de PRs y el motivo de cada
restauración.

## 2. Medición de #65 y #66

El arnés es el banco de tokens del repo y el corpus de #140; no requiere
proveedor para la parte determinista, pero la medición real sí.

1. Con un perfil real configurado (proveedor, modelo y esfuerzo), ejecutar el
   banco de tokens contra el perfil:
   `go test ./internal/review -run '^TestReviewCorpus' -count=1 -v` para la
   parte de calidad, y el chequeo de salud de **Agentes LLM** para latencia.
2. Recoger por perfil: tokens de razonamiento, latencia p95 (objetivo < 30 s) y
   tasa de acierto de caché (`cached_tokens` / `input_tokens`).
3. Anotar en #198 versión de OCR, perfil, tamaño de muestra y fecha. Si el
   proveedor no honra `prompt_cache_key`, documentarlo y mantener el diseño de
   una petición por pasada.

## 3. Ampliar el corpus a 300 PRs

Formato y reglas en
[`internal/review/testdata/corpus/README.md`](../internal/review/testdata/corpus/README.md).

1. Añadir PRs reales por lenguaje (`go`, `typescript`, `python`, `dockerfile`,
   `yaml`, `sql`, `graphql`) y tipo (`code`, `test`, `config`, `infra`,
   `cross_module`, `data`), incluyendo limpios, con defectos conocidos,
   derivaciones y PRs elegibles sin resultado.
2. Registrar la salida real del motor en `reviews/<id>.json` con la procedencia
   exacta (SHAs, versión de OCR y perfil); una observación de otro perfil
   invalida la entrada.
3. Fijar `expected` **antes** de mirar la salida. Las entradas `must_match`
   son regresiones obligatorias; las de medición (`must_match: false`) solo
   alimentan las métricas y documentan la línea base.
4. Recalcular precisión, defectos graves omitidos, exactitud de citas,
   derivaciones útiles/innecesarias/faltantes y PRs sin resultado. El gate de
   tamaño de muestra exige 300 PRs con comentarios evaluados.

## 4. Promoción por clase

1. Elegir una clase acotada (p. ej. hallazgos de arquitectura con cita en el
   head y severidad menor) y publicarla como piloto `COMMENT` mediante
   `architecture_analysis_mode` y la política de publicación.
2. Medir falsos positivos, defectos graves omitidos y derivaciones
   injustificadas solo de esa clase, con la reversión prevista: volver a
   observación es cambiar el modo, sin migración.
3. Decidir por escrito en #198: promote o keep observing, con el umbral usado y
   los datos. Sin datos no hay promoción.

## 5. Huecos conocidos (cerrados en la fase 6)

Editor de consumidores en el panel, contratos YAML (GraphQL sigue `unknown` por
decisión), plazo propio del análisis, citas multiarchivo estructuradas y sonda
de visibilidad real. Cualquier hueco nuevo se anota aquí antes de promover.

## Criterio de cierre de #198

Cada clase promovida tiene evidencia independiente suficiente, la escalera está
rodada y los huecos conocidos están decididos (cerrados o aceptados por
escrito).
