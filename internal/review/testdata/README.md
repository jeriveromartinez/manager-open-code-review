# Fixtures del motor de revisión

Salidas reales de `ocr` y `opencode` usadas para fijar los `json` tags de
`PreviewResult`, `RuleGroup` y del extractor de eventos del agente. Este directorio
solo contiene datos: no incluye parsers ni código de producto.

## Versiones

| Herramienta | Versión / detalle |
| --- | --- |
| `@alibaba-group/open-code-review` (binario `ocr`) | `v1.13.13 (445ad9de) darwin/amd64`, compilado el `2026-11-19T02:49:35Z` |
| `opencode` | `1.19.0` |
| Node / npm | `v24.20.0` / `12.0.2` |
| `jq` | `1.7.1` |

Instalación (prefijo temporal, fuera del repo):

```bash
npm install --prefix /tmp/qa-fixtures @alibaba-group/open-code-review@1.13.13
/tmp/qa-fixtures/node_modules/.bin/ocr --version
# open-code-review v1.13.13 (445ad9de) darwin/amd64
```

npm avisó de que el `postinstall` del paquete quedó bloqueado por la política de
scripts; el binario incluido funciona igualmente y no fue necesario aprobarlo.

## Repo de prueba

Los fixtures de `ocr` se capturaron de un repo git **temporal** (no del repo del
proyecto): `/private/var/folders/8d/3l5hglbj02123b9ftz8w6sj40000gn/T/opencode/ocr-fixture-repo`.

- Commit base `bac725e283dbec23985186cbcc9894bec7981351` (`base: initial fixture`):
  `main.go` y `README.md`.
- Commit HEAD `f567dae75c8e95396f5107dd36e2c6d03beb7c67`
  (`feat: add auth with naive password comparison`): modifica ambos archivos y añade
  `auth.go`, donde `CheckPassword` compara contraseñas con `==` (bug evidente).

## Fixtures y comandos de captura

| Archivo | Contenido |
| --- | --- |
| `ocr-preview.json` | Resultado de `ocr delegate preview`: 3 archivos totales, 2 revisables (`auth.go` añadido, `main.go` modificado) y `README.md` excluido por `unsupported_ext`. |
| `ocr-rule.json` | Resultado de `ocr delegate rule`: 2 grupos (`**/*.go` con `main.go`, `auth.go`; `default` con `README.md`), reglas de sistema en texto. |
| `opencode-events.jsonl` | 3 eventos reales de `opencode run --format json`: `step_start`, `text` (texto `{"ok":true}`) y `step_finish`. |

Comandos ejecutados (con `OCR=/tmp/qa-fixtures/node_modules/.bin/ocr` y
`BASE=bac725e283dbec23985186cbcc9894bec7981351`):

```bash
$OCR delegate preview --format json --repo <repo> --from "$BASE" --to HEAD \
  > internal/review/testdata/ocr-preview.json
$OCR delegate rule --format json --repo <repo> --from "$BASE" --to HEAD main.go auth.go README.md \
  > internal/review/testdata/ocr-rule.json

opencode run --pure --format json \
  --model nvidia/nvidia/nemotron-3.5-lightning-30b-a3b \
  'Responde solo con {"ok":true}' > opencode-events.jsonl
```

La salida de OpenCode se redirigió primero a un archivo temporal y luego se copió a
este directorio; el contenido es el stream completo, sin editar.

### Credencial de OpenCode

La captura de OpenCode **sí usó una credencial real**: la configuración `nvidia` de
`~/.local/share/opencode/auth.json`. Por eso no se aplicó el `HOME=$(mktemp -d)`
del brief: un HOME vacío oculta esa credencial y la ejecución falla. No se necesita
recapturar antes de la Task 12.

## Verificación

```bash
jq . internal/review/testdata/ocr-preview.json > /dev/null
jq . internal/review/testdata/ocr-rule.json > /dev/null
jq -c . internal/review/testdata/opencode-events.jsonl > /dev/null
jq -s 'any(.[]; .type == "text" and (.part.text // "") != "")' \
  internal/review/testdata/opencode-events.jsonl
```

Resultado: los tres archivos parsean; `jq 'keys'` devuelve
`excluded_count, excluded_files, from, merge_base, mode, repository,
reviewable_count, reviewable_files, schema_version, to, total_deletions,
total_files, total_insertions` para `ocr-preview.json` y `groups, schema_version`
para `ocr-rule.json`; el stream tiene 3 eventos y 1 evento `text` con texto.

## Notas para los tests

- `ocr-preview.json` contiene la ruta absoluta del repo temporal y los hashes del
  commit base; son datos de la captura, no valores que los tests deban fijar (salvo
  los `json` tags y la forma de los objetos).
- `opencode-events.jsonl` contiene IDs de sesión/parte y timestamps reales de la
  ejecución; los tests deben comprobar la estructura (`type`, `part.text`), no esos
  valores.
