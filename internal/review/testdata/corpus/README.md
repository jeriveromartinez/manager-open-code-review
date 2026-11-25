# Corpus local de evaluación de revisiones (#140)

Corpus versionado de pull requests limpios y con defectos conocidos, con el
resultado esperado fijado antes de mirar la salida del motor. El arnés
`internal/review/corpus_eval_test.go` reproduce las observaciones registradas a
través de las funciones reales del pipeline (`parseNativeReview`,
`mergeNativeFindings`, `nativeOutcome`, `reviewedPaths`) y calcula métricas por
lenguaje y tipo de cambio. No llama a ningún proveedor, al binario `ocr`, a
GitHub ni a la red; por eso se ejecuta igual en Linux y Windows y no forma parte
de ningún workflow de CI.

## Estructura

```text
internal/review/testdata/corpus/
  corpus.json          manifiesto: versión, OCR, perfil, criterios y entradas
  reviews/<id>.json    salida registrada del doble de OCR para esa comparación
```

Cada entrada de `corpus.json` fija:

- `comparison`: SHA base y head de la comparación revisada.
- `language` y `change_type`: clases de las métricas (`go`, `typescript`,
  `python`, `dockerfile`, `yaml`, `sql`; `code`, `test`, `config`, `infra`,
  `cross_module`, `data`).
- `head_files`: contenido del head usado para validar citas.
- `expected`: resultado esperado (`succeeded`/`inconclusive`/`failed`), si la
  derivación es esperada y con qué motivo, archivos revisables y hallazgos
  esperados con ruta, categoría, severidad y rango.
- `observation`: ruta de la salida registrada del doble de OCR y estado de
  entrega (`published`, `failed` o `not_applicable`). Sin `review` la entrada
  representa un PR elegible que quedó sin resultado.

La procedencia se verifica en cada observación: `manifest.input.resolved_base`
y `resolved_head` deben coincidir con `comparison`, y
`manifest.execution.ocr_version`/`model` con la versión de OCR y el perfil del
manifiesto. Una observación vieja o de otro perfil invalida la entrada.

`must_match` marca la regresión obligatoria: su expectativa debe coincidir con
la observación. Las entradas de medición (`must_match: false`) registran de
forma deliberada un defecto grave omitido, un falso positivo, una derivación
innecesaria o un PR sin resultado; existen para ejercitar las métricas y
documentan la línea base actual, no una aceptación del comportamiento.

## Ejecución

```sh
export TESTCONTAINERS_RYUK_DISABLED=true   # no se usa PostgreSQL, pero deja el entorno listo
go test ./internal/review -run '^TestReviewCorpus' -count=1 -v
```

En PowerShell:

```powershell
$env:TESTCONTAINERS_RYUK_DISABLED = "true"
go test ./internal/review -run '^TestReviewCorpus' -count=1 -v
```

El test imprime el informe JSON completo. Los tests comprueban que:

- el corpus se evalúa dos veces con el mismo resultado (determinismo);
- una expectativa deliberadamente errónea falla la entrada y el gate;
- los totales se derivan de los fixtures (comentarios, hallazgos esperados,
  fallos de entrega y PRs sin resultado se recuentan por separado);
- cada observación registra tamaño de muestra, versión de OCR y perfil;
- el corpus cubre las lenguas y clases exigidas.

## Métricas

Por entrada y agregadas por `language` y por `change_type`:

| Métrica | Definición |
| --- | --- |
| `prs_with_evaluated_comments` | PRs con al menos un comentario de defecto observado; es el tamaño de muestra del gate de autonomía. |
| `coverage` | archivos revisables presentes en `coverage.completed`/`reused` ÷ archivos revisables esperados. Los PRs sin resultado se cuentan aparte. |
| `precision` | comentarios que coinciden con un defecto esperado ÷ comentarios de defecto observados. Un comentario en un PR limpio siempre es falso positivo. |
| `detection` | defectos esperados detectados ÷ defectos esperados. |
| `missed_severe_defects` | defectos esperados `major`/`blocker` no detectados. |
| `severity_mismatches` | defectos detectados cuya severidad observada (mapeada al vocabulario del panel) difiere de la esperada. |
| `citation_accuracy` | citas cuyo `existing_code` aparece en el rango citado del archivo head ÷ citas comprobadas. Una cita fuera de rango o de un archivo ausente es inexacta. El `cited_code` esperado también se valida contra el head. |
| `clean_classified_defective` | PRs esperados limpios con al menos un comentario de defecto observado. |
| `eligible_prs_without_result` | PRs elegibles sin ejecución registrada. |
| `delivery_failures` | observaciones con entrega fallida; se excluyen de las derivaciones confirmadas. |
| `useful_escalations` | derivaciones esperadas cuyo motivo coincide con el observado. |
| `unnecessary_escalations` | derivaciones observadas en PRs que el corpus esperaba concluyentes. |
| `missing_escalations` | derivaciones esperadas que la observación dio por concluidas. |
| `escalation_sample_size` | PRs evaluados para derivación (con resultado y sin fallo de entrega). |
| `escalation_reasons` | motivos observados (`coverage_incomplete`, `config`, `timeout`, ...). |

Una coincidencia exige misma ruta normalizada, misma categoría del panel y
rangos solapados o a menos de `contextRadius` líneas. La severidad no decide la
coincidencia, pero la severidad mapeada del hallazgo observado se compara con la
esperada y una diferencia cuenta como `severity_mismatches` y falla la entrada.
Un denominador cero se informa como 1 (vacío perfecto), no como `NaN`.

La procedencia es tri-estado: `verified` para una observación comprobada contra
la comparación, OCR y perfil; `unrecorded` para un PR sin observación; y
`mismatch` cuando algún dato no coincide. Un PR sin resultado no figura como
procedencia verificada.

## Línea base

Primera medición de `2026-11-24.1` con OCR `v1.14.0` (build `d29ca204`) y perfil
falso `fake/fake-model` (15 PRs, de ellos 14 con resultado y 9 con comentarios
evaluados): cobertura 15/17 archivos,
9 comentarios observados con 8 coincidencias y 1 falso positivo, 8/9 defectos
graves detectados, 8/9 citas exactas, 1 PR limpio clasificado como defectuoso,
1 PR elegible sin resultado, 1 fallo de entrega, 1 derivación útil y 1
innecesaria. El propio arnés calcula estas cifras; no están codificadas en el
test.

## Criterios de autonomía

El corpus permanece en observación hasta que la medición cumpla, sobre
`must_match` y sobre el conjunto ciego prospectivo:

- 100 % de los archivos revisables cubiertos, 100 % de entrega de hallazgos
  confirmados, 0 duplicados y 0 comentarios obsoletos.
- 0 falsos positivos, 0 defectos `major`/`blocker` omitidos y 0 PRs limpios
  clasificados como defectuosos.
- 0 PRs elegibles sin resultado y 0 fallos de entrega confundidos con
  derivaciones.
- 100 % de exactitud de citas contra el SHA vigente.
- 0 derivaciones innecesarias y motivo correcto en las útiles.
- Mínimo 300 PRs independientes con comentarios evaluados. Como referencia
  estadística (solo referencia, no umbral de aprobación), 0 falsos positivos en
  300 PRs independientes da un límite superior unilateral del 95 % de ~1 % para
  PRs con al menos un falso positivo; un cero observado no garantiza una tasa
  poblacional cero.

Los umbrales se evalúan en `autonomy_criteria` y el informe lista cada
incumplimiento. Si aparece un falso positivo, un bloqueante omitido como limpio
o falta cobertura representativa, la clase sigue en observación, se corrige y
se vuelve a medir con un conjunto ciego. Solo entonces se puede habilitar
publicación `COMMENT` en un piloto autorizado con auditoría y reversión; usar el
dictamen en el gate es una decisión posterior.

## Qué no se puede medir offline

- La precisión y la detección de un modelo real: las observaciones son salidas
  guionizadas del doble y del proveedor falso, no una evaluación ciega.
- El comportamiento del proveedor (latencias, reintentos, límites, coste real)
  y el consumo de tokens/tiempo del motor real.
- Diferencias del binario `ocr` real, del sistema de archivos o de la red.
- La entrega real en GitHub, los hilos, el check y las carreras entre pushes.
- La revisión humana independiente y la adjudicación de desacuerdos.
- La muestra prospectiva consecutiva de PRs elegibles: el corpus local solo
  fija las regresiones y las definiciones de las métricas.
