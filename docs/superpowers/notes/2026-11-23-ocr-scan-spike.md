# Spike: `ocr scan` para la verificación de hallazgos

Fecha: 2026-11-23. OCR 1.14.1 (el worker avisa de 1.14.2 disponible; no se sube).

## Qué se ejecutó

`docker --context kiri exec deploy-worker-1 ocr scan --help`, solo lectura.
No se hizo una llamada real al modelo: el host no tiene el binario `ocr` y una
llamada viva exige la clave maestra del worker y un perfil configurado. La
adherencia del modelo al prefijo `QA-VERIFY` se valida en el despliegue
`dry_run`, como ya preveía el diseño.

## Flags aceptadas por `ocr scan`

```
-b, --background string       contexto de requisitos (texto, no fichero)
    --format string           text | json | sarif (usar json)
    --max-tokens int          techo de tokens por prompt
    --max-tokens-budget int   tope total de tokens (0 = sin límite)
    --model string
    --path string             rutas relativas separadas por comas
    --provider string
    --repo string             raíz del repositorio (por defecto, cwd)
    --rule string             ruta a un JSON de reglas del sistema
    --timeout int             presupuesto por tarea concurrente, en minutos
    --no-plan                 omite el pre-paso PLAN_TASK por archivo
    --concurrency int
    --exclude string
    --audience string
    --preview
    --resume string
```

Correcciones que este spike impone al plan:

- `scan` **no** acepta `--background-file`; usa `--background <texto>`. El
  contexto de verificación va en línea, no en un fichero (el worker corre en
  Linux, donde el límite de argv es amplio; el plan acota los hallazgos).
- `scan` **no** acepta `--effort`. La política `verify_effort` no se puede
  aplicar a esta pasada y se elimina del diseño.
- `--max-tokens`, `--max-tokens-budget` y `--timeout` sí existen, así que el
  presupuesto de verificación del plan se mantiene.

## Forma de la salida

El sobre es el mismo que `ocr review --format json` y lo consume
`parseNativeReview`:

- Claves de nivel superior: `status`, `llm`, `message`, `summary`,
  `tool_calls`, `comments`, `groups`, `session_id`, `manifest`.
- Cada comentario: `path`, `content`, `suggestion_code`, `existing_code`,
  `start_line`, `end_line`, `category`, `severity`.
- `manifest.operation` es `scan` y `manifest.input.mode` es `full` (no
  `range`).

El fixture `internal/review/testdata/ocr-scan-complete.json` fija esa forma con
tres veredictos: un `CONFIRM`, un `REFUTE` con evidencia presente en el código
citado y un `UNCERTAIN` sin evidencia.

## Contrato de veredicto

La primera línea no vacía de cada comentario debe ser
`QA-VERIFY <finding_id> <CONFIRM|REFUTE|UNCERTAIN>`; la segunda,
`EVIDENCE: <una línea copiada literalmente del archivo>`. Sin evidencia que
contradiga la afirmación, el veredicto es `UNCERTAIN`. El manager degrada a
`UNCERTAIN` cualquier `REFUTE` cuya evidencia no aparezca en el archivo del
head, así que un prefijo incumplido o una alucinación nunca descartan nada.
