# Prueba de extremo a extremo

`TestE2EFlow` recorre el flujo completo —trigger, revisión, publicación y eventos del panel— contra dobles locales. No necesita una GitHub App real, ni una API key de un LLM, ni red: usa un repositorio bare local, los dobles `ocr`/`opencode` de `internal/review/testdata` y un servidor HTTP que emula los endpoints de GitHub que consume la publicación.

## Comando

En Linux, con Docker disponible (PostgreSQL vía `testcontainers`):

```sh
go test ./internal/review -run '^TestE2EFlow$' -count=1 -v
```

En Windows con Docker en la distro Kiri:

```sh
MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'cd /mnt/e/Projects/manager-open-code-review; export PATH=/usr/local/go/bin:/usr/bin:/bin; export GOFLAGS=-buildvcs=false; export DOCKER_HOST=tcp://127.0.0.1:2375; go test ./internal/review -run "^TestE2EFlow$" -count=1 -v'
```

El test forma parte de `go test ./...`; el comando anterior solo lo aísla. La regresión del timeout de River vive en `internal/jobs` (`TestReviewClientJobTimeoutOutlivesRiverDefault`).

## Casos cubiertos

| Caso | Qué se comprueba |
|---|---|
| Ejecución correcta | Resumen creado con el marcador, review con el comentario inline, check `failure` (hallazgo bloqueante) y PR `changes_requested`. El `ocr` recibe el SHA base resuelto de `FETCH_HEAD`, no el nombre de rama. |
| Timeout | Run `inconclusive`/`timeout`, check `action_required`, PR `inconclusive`, resumen con el aviso y hallazgos abiertos intactos. |
| Configuración inválida y reintento | Run `failed`/`config`, check `action_required`, PR `inconclusive`; el reintento reabre el mismo check, publica resumen y termina en `changes_requested`. |
| Push antes del checkout | Run `superseded`/`head_moved`, check `cancelled`, PR sin cambios, ningún resumen y solo `run.updated`; el nuevo head se revisa y publica. |
| Ciclo de un hallazgo | Runs `inconclusive` y `failed` no resuelven nada; el siguiente run correcto resuelve el hallazgo, cierra el hilo, responde y edita el mismo comentario resumen. |

Cada transición terminal se verifica también contra un `events.Broker` real (LISTEN/NOTIFY de PostgreSQL): `run.updated` y, salvo en `superseded`, `pr.updated`. El caso correcto además lee el stream SSE (`GET /api/events`) para comprobar que el panel se actualiza sin recargar.

## Cómo se inyectan los fallos

Los dobles se controlan con variables de entorno (`RUN_FAKE_AGENT_FAIL`, `RUN_FAKE_AGENT_SLEEP_ON_CALL`, `RUN_FAKE_AGENT_OUTPUT`, `RUN_FAKE_AGENT_CONFIG_OUT`, `RUN_FAKE_OCR_ARGV`) descritas en `internal/review/testdata/README.md`; el resto de fallos se provoca desde el estado de PostgreSQL (perfil deshabilitado) o desde el servidor falso de GitHub.

## Banco de consumo de tokens

`internal/review/tokenbank_test.go` ejecuta el binario real de OpenCode 1.19 contra `internal/testutil.FakeProvider`, un endpoint OpenAI Chat Completions falso que registra cada petición (mensajes, herramientas, marcas de caché y tokens estimados) y devuelve respuestas guionizadas. No hay llamadas a un proveedor real ni coste de tokens.

| Test | Qué fija |
|---|---|
| `TestTokenBankNoReadsMakesOneRequest` | Una pasada sin lecturas hace una sola petición y declara solo `read`. Detecta la llamada de título y las herramientas de exploración. |
| `TestTokenBankStopsAtStepBudget` | Con el tope por defecto, el bucle recibe el aviso de último paso en la octava petición y ninguna petición supera 270k tokens de contexto. |
| `TestTokenBankReferenceFigures` | Reproduce el caso del diagnóstico (#76) con 12 lecturas y un diff de 30 KB; falla si el consumo total se sale del orden de magnitud esperado. |

Los tres se saltan con un mensaje claro si `opencode` no está instalado o no es 1.19.x. En el host de desarrollo suele haber otra versión, así que el banco se ejecuta dentro de la imagen del worker. Se compila el binario de test para Linux en Windows y se monta en la imagen; no hace falta PostgreSQL ni WSL:

```powershell
$env:GOOS = "linux"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go test -c -o "$env:TEMP\qa-review.test" ./internal/review
docker --context kiri run --rm `
  -v "$($env:TEMP)\qa-review.test:/review.test:ro" `
  --entrypoint /review.test qa-control-center-worker:local `
  -test.run '^TestTokenBank' -test.v
```

En Linux o macOS:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/qa-review.test ./internal/review
docker --context kiri run --rm \
  -v /tmp/qa-review.test:/review.test:ro \
  --entrypoint /review.test qa-control-center-worker:local \
  -test.run '^TestTokenBank' -test.v
```

Medición de referencia con la imagen `qa-control-center-worker:local` (OpenCode 1.19.0) el 2026-11-21: la pasada sin lecturas hace 1 petición; el bucle con lecturas se detiene en 8 peticiones con un contexto máximo de ~46k tokens; el caso de 12 lecturas termina en 13 peticiones, ~56k de contexto máximo y ~436k tokens estimados en total. La cifra exacta depende del diff y de los archivos; el test usa un margen amplio para detectar regresiones de orden de magnitud, no para fijar un número.

El banco no se ejecuta en GitHub Actions ni en ningún pipeline: es una verificación local, igual que el resto de la suite.
