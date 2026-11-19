# QA Control Center — Diseño

Fecha: 2026-11-18 · Estado: borrador para aprobación

## 1. Intención

Los team leads y QA de equipos de desarrollo pierden mucho tiempo revisando cada iteración de cada PR en muchos repositorios. QA Control Center es una plataforma **self-hosted y open source**, muy parecida a CodeRabbit, que revisa los PRs de forma autónoma con el LLM que cada organización elija (incluida una suscripción usada vía OpenCode), comenta hallazgos y sugerencias en el PR después de **cada** revisión, repite el ciclo con cada corrección del desarrollador, y **solo cuando ya no encuentra problemas** notifica al QA/lead para la revisión final y el merge, que siguen ocurriendo en GitHub.

Motor de análisis: [alibaba/open-code-review](https://github.com/alibaba/open-code-review) (`ocr`, npm `@alibaba-group/open-code-review`) en **Delegation Mode**, con **OpenCode CLI** como agente que razona. No se modifica el código de ninguno de los dos.

## 2. Usuarios y roles

| Rol | Puede |
|---|---|
| Admin de instancia | Instalar/configurar GitHub App, perfiles LLM, equipos, ver todo, auditoría |
| QA / Team lead (por equipo) | Políticas de QA de sus repos, confirmar/descartar hallazgos bloqueantes, reintentar revisiones, recibir solicitudes de revisión final |
| Desarrollador | Ver PRs y hallazgos de los repos de sus equipos; disputar hallazgos respondiendo en el PR |

Una instancia sirve a varios equipos de una organización. Cualquier otra organización instala su propia instancia.

## 3. Objetivos, no-objetivos, anti-objetivos

**Objetivos (v1)**
- Instalar la GitHub App en una org y empezar a revisar los repos seleccionados sin copiar workflows.
- Revisión automática en `opened`, `synchronize`, `reopened`, `ready_for_review`; importar PRs abiertos al instalar.
- Comentario de resumen + comentarios en línea con sugerencias aplicables tras cada revisión.
- Ciclo autónomo revisión → corrección → re-revisión, con hallazgos resueltos marcados y sus hilos cerrados.
- Quality Gate y solicitud de revisión al QA/lead solo cuando el PR está limpio.
- Panel web en vivo (tipo NOC) multi-equipo con roles.
- Cada equipo elige su perfil LLM (suscripción vía OpenCode o API key de un proveedor).

**No-objetivos (v1)**
- GitLab/Bitbucket (se deja una interfaz de proveedor SCM, solo implementación GitHub).
- Ejecutar pruebas propias: se usa el CI existente del repo como señal.
- Autocorrección de código y merge automático.
- SaaS multi-organización alojado.
- Slack/email (la notificación es la solicitud de revisión de GitHub + el panel).

**Anti-objetivos (sería un fracaso aunque funcione)**
- Que un PR se marque listo sin que se haya revisado el SHA exacto actual.
- Que un fallo del LLM / cuota agotada se trate como aprobación.
- Que el agente pueda modificar el repo, ejecutar código del PR o filtrar credenciales.
- Ruido: un comentario nuevo por push en lugar de actualizar el resumen; repetir hallazgos ya resueltos.
- Que un desarrollador pueda "convencer" al bot de retirar un bloqueante sin que un humano autorizado lo confirme.

## 4. Restricciones

- Self-hosted con Docker Compose; instalación inicial en ~15 minutos por organización.
- Stack decidido: **Go** (backend), **Vue 3 + Vite + TypeScript** (web), **PostgreSQL + GORM**, **SSE** para tiempo real, **GitHub App**.
- `ocr delegate` solo tiene `preview` y `rule`; la revisión la hace el agente host (OpenCode).
- La GitHub Action oficial de `ocr` requiere token de LLM, no sirve para suscripciones: no se usa.
- Términos de uso: algunas suscripciones de consumo prohíben uso compartido/automatizado; por eso cada perfil LLM puede ser suscripción u API key, y la UI muestra un aviso al configurar una suscripción.

## 5. Enfoques considerados

| Enfoque | Resumen | Decisión |
|---|---|---|
| A. Arquitectura original | `qa-api`, `qa-worker`, `qa-web`, `qa-scheduler` + PostgreSQL + Redis/Asynq | Descartado: 6 contenedores y dos almacenes de estado frenan la adopción self-hosted |
| **B. Binario Go único con roles** | Una imagen: `qa serve` (API + web embebida + scheduler) y `qa worker`; cola en PostgreSQL con River | **Elegido** por el usuario: 3 contenedores, mismo comportamiento, worker aislado |
| C. SaaS centralizado | Alojado por el autor | Descartado: custodiar código y credenciales ajenas |

Motor: un solo camino (Delegation Mode + `opencode run`), porque OpenCode ya soporta tanto suscripciones como API keys. No se usa el modo nativo `ocr review`.

## 6. Arquitectura

```
GitHub ──webhooks──▶ qa serve ──(River jobs en Postgres)──▶ qa worker ×N
   ▲                   │  API REST + SSE + Vue embebido        │ clona, ocr delegate, opencode run
   └──── comentarios, check runs, review requests ◀────────────┘
                       PostgreSQL (estado, cola, auditoría)
```

**Contenedores (docker-compose):** `app` (`qa serve`), `worker` (`qa worker`, escalable), `postgres`.
- `app`: webhooks, API REST, SSE, web Vue embebida con `go:embed`, scheduler (reconciliación periódica). Tiene la clave privada de la GitHub App.
- `worker`: ejecuta revisiones. Tiene `git`, `ocr` (npm, versión fijada) y `opencode` (versión fijada) instalados. Recibe del `app`, vía el job, un token de instalación de corta duración solo para clonar; la auth de OpenCode vive en un volumen propio del worker.
- El worker **no** publica en GitHub: devuelve el resultado estructurado y un job de publicación en `app` comenta/actualiza checks. Así el token con permisos de escritura nunca está en el contenedor donde se procesa código ajeno.

**Webhooks e instalaciones sin URL pública:** el scheduler reconcilia cada N minutos (configurable, por defecto 5) listando PRs abiertos de los repos activos y encolando lo que falte. Cubre webhooks perdidos e instancias sin entrada pública (modo "solo polling"). *(Decisión del diseñador)* El modo solo polling cubre revisiones y estado de CI (consulta checks de los PRs en `waiting_ci`); disputas en hilos y comandos `/qa` requieren webhooks y así se documenta. En modo polling, descartes y confirmaciones se hacen desde el panel.

**Estructura del repo (propuesta):**
```
cmd/qa/            main: subcomandos serve, worker, migrate
internal/github/   cliente App, webhooks, comentarios, checks (interfaz SCMProvider)
internal/review/   pipeline: preview, rule, prompt, opencode, parseo, fingerprints
internal/gate/     Quality Gate
internal/jobs/     definiciones River
internal/store/    modelos GORM + migraciones
internal/api/      REST + SSE + auth
internal/auth/     OAuth GitHub, sesiones, RBAC
web/               Vue 3 + Vite + TS
deploy/            docker-compose.yml, Dockerfile, .env.example
```

## 7. GitHub App

**Permisos:** Pull requests R/W, Checks R/W, Contents R, Metadata R, Commit statuses R, Members R (org, para mapear GitHub Teams), Issues R/W (comentarios de PR usan la API de issues).
**Eventos:** `installation`, `installation_repositories`, `pull_request`, `pull_request_review_comment` (respuestas/disputas), `issue_comment` (comandos `/qa ...`), `check_suite`, `check_run`, `status` (CI terminó → reevaluar gate).
**Login web:** OAuth de usuario de la misma GitHub App. El primer usuario que completa el setup es admin de instancia.
**Setup guiado:** el panel ofrece el flujo de "GitHub App manifest" para crear la App con permisos/eventos correctos en un clic y guardar credenciales.

## 8. Pipeline de revisión

Estados de un PR en el sistema:
`awaiting_approval → pending → reviewing → changes_requested | waiting_ci | ready_for_human | inconclusive`, más `merged`/`closed`. `draft` se ignora hasta `ready_for_review`.

**Disparo:** cada evento relevante crea un `ReviewRun` atado a `(pr, head_sha)`. Un push nuevo cancela el run en curso del mismo PR (superseded), invalida el estado listo anterior y, si había solicitud de revisión del sistema, la retira para volver a pedirla cuando quede limpio. Solo un run activo por PR.

**Pasos del worker:**
1. Clonar en directorio efímero (`git fetch` del base y del head SHA, profundidad suficiente para el merge-base). Borrar credenciales de `.git/config` tras el fetch.
2. `ocr delegate preview --format json --from <merge_base> --to <head_sha> [--exclude ...]` → lista de archivos revisables, excluidos, cobertura.
3. `ocr delegate rule --format json --from ... --to ... <archivos>` → reglas agrupadas. Precedencia de reglas: regla del panel (escrita a archivo temporal, `--rule`) > `.opencodereview/rule.json` del repo > defaults de `ocr`.
4. Generar el diff (`git --no-pager diff --no-ext-diff --no-textconv --no-color`) y construir el prompt: instrucciones de revisión, reglas por grupo, contexto del PR (título, descripción) como **datos no confiables**, hallazgos abiertos de la revisión anterior (para marcarlos resueltos o vigentes) y el esquema JSON de salida.
5. `opencode run --format json --agent qa-reviewer --model <perfil> [--variant ...] --dir <tmp>` con un agente de solo lectura: `edit` deny, `shell` deny, `webfetch` deny; lectura/búsqueda de archivos permitida para dar contexto. Nunca `--auto`.
6. Extraer la respuesta final de los eventos JSON, validarla contra el esquema; si no valida, un reintento pidiendo corrección; si vuelve a fallar → `inconclusive`.
7. Guardar resultado, encolar job de publicación y evaluación del gate.

**Esquema de salida (propio):**
```json
{
  "summary": "string",
  "coverage": {"reviewed_files": 0, "skipped_files": 0},
  "previous_findings": [{"id": "uuid", "status": "resolved|still_present"}],
  "findings": [{
    "path": "string", "start_line": 0, "end_line": 0,
    "severity": "blocker|major|minor|nit",
    "category": "bug|security|performance|maintainability|style|test",
    "title": "string", "body": "string",
    "suggestion": "código de reemplazo opcional",
    "confidence": "high|medium|low"
  }]
}
```
Cada hallazgo recibe un fingerprint (ruta + categoría + título normalizado + contexto de código) para deduplicar entre revisiones y no repetir lo ya reportado.

**Cobertura completa** *(decisión del diseñador)*: todos los archivos revisables (los que `ocr delegate preview` no excluye por política explícita) deben quedar revisados. Los PRs grandes se dividen en varias pasadas de `opencode run` usando los grupos de `ocr delegate rule` y un presupuesto de tamaño por pasada; los hallazgos se fusionan. Si el LLM reporta archivos omitidos se reintentan en una pasada adicional; si aún faltan, el run es `inconclusive` (nunca listo) y el panel lo indica para que un QA/lead decida.

**Fallos:** errores de LLM/cuota/timeout se reintentan con backoff (3 intentos); después `inconclusive`, check `neutral`, alerta en el panel. Nunca aprobado.

**Concurrencia:** límite por perfil LLM (configurable) además del número de workers.

## 9. Publicación en el PR

- **Comentario de resumen** único por PR, identificado con un marcador oculto y **editado en cada revisión**: número de revisión, SHA, conteo por severidad, resueltos desde la anterior, resumen del cambio, lista de hallazgos con enlaces, estado del CI.
- **Review con comentarios en línea** solo para hallazgos nuevos; si la sugerencia cae dentro de un hunk del diff se usa bloque ```` ```suggestion ````, si no, texto.
- **Hallazgos resueltos:** respuesta corta en su hilo + resolver el hilo (GraphQL `resolveReviewThread`).
- **Check run** "QA Control Center" por SHA: `in_progress` mientras revisa o espera CI → `success` solo en `ready_for_human`; `failure` con hallazgos bloqueantes; `action_required` cuando es inconcluso o está pausado. *(Decisión del diseñador)* Nunca `neutral`, porque GitHub lo trata como aprobado; la guía de instalación recomienda marcar este check como requerido en branch protection.
- Se escapan `@menciones` del texto generado por el LLM para evitar pings inyectados.

## 10. Disputas y descartes

- El desarrollador responde en el hilo de un hallazgo → job de reevaluación con el argumento → el LLM responde `uphold`, `withdraw` o `downgrade`, y lo explica en el hilo.
- Si retira o rebaja un hallazgo **no bloqueante**, se aplica directamente.
- Si el hallazgo era **bloqueante**, pasa a `pending_confirmation`: sigue bloqueando hasta que un QA/lead del equipo lo confirme desde el panel o con `/qa dismiss <id>` en el PR (se valida que el autor del comando tenga ese rol).
- QA/lead pueden descartar cualquier hallazgo en cualquier momento. Todo queda en auditoría.
- Comandos v1: `/qa approve` (autorizar PR externo), `/qa review` (forzar re-revisión), `/qa dismiss <id>`, `/qa pause`, `/qa resume`.

## 10b. PRs de forks y autores externos (en alcance v1)

Decisión del usuario: se revisan repos públicos y PRs de forks/contribuidores externos.

- **Autor confiable:** `author_association` = `OWNER`, `MEMBER` o `COLLABORATOR` (configurable en la política). Se revisa automáticamente.
- **Autor externo:** el PR entra en estado `awaiting_approval` (check `action_required`, comentario breve explicando que espera aprobación). No se gasta LLM hasta que un QA/lead/maintainer del equipo lo aprueba con `/qa approve`, desde el panel o con la etiqueta `qa:approved`.
- *(Decisión del diseñador)* La aprobación vale para ese PR; los pushes posteriores se revisan sin re-aprobar, pero sujetos a límites. La política puede exigir re-aprobación en cada push.
- **Límites** *(decisión del diseñador, valores por defecto configurables)*: por autor externo, máximo de runs por día (10) y tamaño máximo de diff (p. ej. 3.000 líneas cambiadas); por repo, presupuesto diario de runs. Al superarlos el PR pasa a `awaiting_approval` de nuevo.
- **Obtención del código:** siempre desde el repo base vía `refs/pull/<n>/head`, nunca clonando el fork.
- En repos públicos los comentarios del bot son públicos: el saneamiento y la redacción de secretos de §14 se aplican igual, y el resumen no incluye datos internos (nombres de perfiles LLM, costos, logs).

## 11. Quality Gate

Un PR pasa a `ready_for_human` cuando **todo** se cumple:
1. El último run completado corresponde al head SHA actual.
2. No hay hallazgos abiertos con severidad ≥ umbral bloqueante de la política (por defecto `major`), incluidos los `pending_confirmation`.
3. Checks de CI requeridos en verde en ese SHA. Por defecto: todos los checks del SHA terminados y ninguno fallido (excluyendo el propio); la política puede listar checks requeridos concretos.
4. El PR no es draft.

Si 1–2 se cumplen pero el CI no ha terminado → `waiting_ci`, reevaluado al llegar `check_suite`/`status`.

Al pasar a listo: check `success`, solicitud de revisión a los revisores de la política (usuarios o GitHub Team del QA/lead) —esto genera la notificación oficial de GitHub— y evento SSE + notificación en el panel.

## 12. Panel web (Vue 3)

| Pantalla | Contenido |
|---|---|
| Dashboard | KPIs (abiertos, esperando revisión humana, en evaluación, con problemas, inconclusos), lista de PRs filtrable (Todos/En revisión/Bloqueados/Listos), actividad en vivo |
| Repositorios | Repos de la instalación, activar/desactivar, asignar a equipo |
| Pull Requests / Detalle | Historial de runs por SHA, hallazgos con estado, disputas, diff con hallazgos anotados, CI, logs del run, acciones (reintentar, descartar, confirmar) |
| Agentes LLM | Perfiles: proveedor/modelo de OpenCode, variante, modo (suscripción/API key), concurrencia, prueba de salud |
| Configuración QA | Por repo/equipo: umbral bloqueante, checks requeridos, revisores finales, reglas `ocr`, instrucciones extra, exclusiones, perfil LLM |
| Workers | Jobs en curso/fallidos, duración, consumo de tokens si OpenCode lo reporta |
| Analítica | Hallazgos por categoría/desarrollador/repo, revisiones hasta quedar limpio, tiempo hasta listo, tasa de inconclusos |
| Auditoría | Cambios de políticas, descartes, reintentos, confirmaciones |
| Equipos | Equipos, miembros y roles; sincronización opcional con GitHub Teams |

Los desarrolladores solo ven repos de sus equipos. Referencia visual del Dashboard (propuesta del usuario): cabecera "QA Control Center · Servicio activo", 4 KPIs (PRs abiertos, Esperando tu revisión, En evaluación, Con problemas), filtros Todos/En revisión/Bloqueados/Listos y tarjetas por PR con `repo #n`, etiqueta de estado (Listo para ti, Revisando IA, N defectos, Pruebas fallidas), título, "x/y validaciones correctas" y estado de acción (Pendiente de aprobación, Análisis en curso, Requiere corrección).

*(Decisión del diseñador)* "x/y validaciones" cuenta las condiciones del gate expandidas: cada check de CI requerido + SHA actual revisado + cobertura completa + sin hallazgos bloqueantes abiertos. La API las expone como lista para mostrarlas en el detalle.

**Equipos, repos y políticas** *(decisión del diseñador)*: un repo pertenece a exactamente un equipo. La política se define a nivel de equipo y cada repo puede sobrescribir campos concretos (fusión campo a campo; el valor del repo gana). Los QA/lead con autoridad sobre un repo son los del equipo dueño.

## 13. Perfiles LLM y credenciales

- **API key:** guardada cifrada en Postgres (AES-GCM, clave maestra por variable de entorno) e inyectada como variable de entorno solo al proceso `opencode` del run.
- **Suscripción:** el admin autentica OpenCode dentro del contenedor worker (`docker compose exec worker qa llm login <perfil>`, que envuelve `opencode auth login`); la auth persiste en el volumen del worker. La UI muestra el estado (prueba de salud) y el aviso de términos de uso.
- *(Decisión del diseñador)* **Varios perfiles de suscripción a la vez:** cada perfil tiene su propio directorio de datos de OpenCode (`XDG_DATA_HOME` por perfil) en un volumen compartido por los workers. Para evitar carreras al renovar tokens OAuth, las ejecuciones de un perfil de suscripción toman un lock en Postgres (advisory lock) durante el arranque de `opencode`; su concurrencia por defecto es 1 y es configurable.
- La configuración de OpenCode del worker (agente `qa-reviewer` con permisos de solo lectura) la genera el sistema; no depende de la config del usuario.

## 14. Seguridad

- El código del PR nunca se ejecuta: solo se lee (git, `ocr`, herramientas de lectura de OpenCode).
- El agente no puede editar, ejecutar shell ni acceder a la red salvo la API del LLM.
- *(Decisión del diseñador)* **Defensa en profundidad contra exfiltración de credenciales** vía prompt injection (p. ej. que el agente lea `auth.json` y lo meta en un hallazgo): (a) permiso `external_directory` de OpenCode en deny, para que solo lea dentro del checkout; (b) cada run corre como usuario de sistema sin privilegios cuyo HOME es temporal y solo contiene una copia de la auth del perfil; el checkout es de solo lectura para ese usuario; (c) antes de publicar, toda salida del LLM se escanea contra los valores secretos conocidos (tokens del perfil, token de clonado) y patrones de credenciales, y se redacta; si hay coincidencia, el run se marca y se alerta en el panel. Aislamiento por contenedor por run queda fuera de v1 (exigiría socket Docker en el worker).
- El contenedor worker no tiene la clave de la GitHub App ni tokens con escritura; el token de clonado es de corta duración y se borra del checkout.
- Contenido del PR (título, descripción, código, comentarios) se trata como datos no confiables en el prompt; la salida del LLM se valida contra esquema y se sanea antes de publicar.
- Webhooks verificados con firma HMAC. Secretos cifrados en reposo. Directorios temporales eliminados al terminar.

## 15. Modelo de datos (resumen)

`installations`, `repositories`, `teams`, `team_members`, `users`, `qa_policies`, `llm_profiles`, `pull_requests`, `review_runs` (por SHA), `findings`, `finding_events` (disputas/descartes), `github_artifacts` (ids de comentarios, reviews, check runs), `notifications`, `audit_log`, tablas de River.

## 16. Fases

1. **Núcleo:** binario, migraciones, GitHub App (manifest + webhooks + reconciliación), login y roles, repos/equipos, Dashboard y lista de PRs en vivo (SSE).
2. **Motor:** worker, pipeline `ocr delegate` + `opencode run`, perfiles LLM, comentario de resumen, comentarios en línea, check runs, re-revisión por push y resolución de hilos.
3. **Gate y ciclo humano:** Quality Gate con CI, solicitud de revisión, disputas/descartes y comandos, inconclusos y reintentos, Configuración QA en la web.
4. **Operación:** Workers, Analítica, Auditoría completa, endurecimiento y documentación de instalación.

## 17. Pruebas del propio sistema

- Go: unit tests por paquete; integración con GitHub simulado (`httptest`) y binarios falsos de `ocr`/`opencode` que devuelven fixtures JSON.
- Vue: Vitest para componentes y stores.
- Un e2e con docker-compose y repo de ejemplo usando los binarios falsos.

## 18. Pendiente de verificar al construir

- Campos exactos del JSON de `ocr delegate preview/rule` (la documentación no publica una muestra): capturar fixtures reales en la fase 2.
- Formato de eventos de `opencode run --format json` y cómo extraer el mensaje final y el uso de tokens.
- Comportamiento de permisos `ask` en `opencode run` no interactivo (debe resultar en denegación, nunca en bloqueo).
- Permiso exacto que requiere `resolveReviewThread` para una GitHub App.

## 19. Decisiones dejadas al constructor

Librerías Go concretas (router HTTP, cliente GitHub, River vs alternativa equivalente sobre Postgres), librería de componentes Vue, redacción exacta de prompts, diseño visual detallado del panel, intervalos y límites por defecto no fijados aquí.
