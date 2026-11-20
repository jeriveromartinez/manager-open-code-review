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
