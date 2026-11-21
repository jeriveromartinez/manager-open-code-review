# Modelos cargados desde la API de cada plataforma

**Fecha:** 2026-11-21
**Estado:** aprobado

## Problema

El catálogo de plataformas de `web/src/llmPlatforms.ts` lleva los modelos
escritos a mano. Esos identificadores cambian con frecuencia (nuevas
generaciones, retiradas, alias) y una lista estática envejece en semanas. El
usuario ya vio el problema: faltaban OpenCode, Kimi, GLM y CommandCode.

## Objetivo

Al crear un perfil contra una plataforma conocida, cargar los modelos
disponibles desde la API de esa plataforma usando la clave que el usuario
acaba de pegar, para que el select muestre lo que esa cuenta puede usar hoy.

**No objetivo:** consultar la API para perfiles ya guardados con la clave
cifrada; no se amplía la superficie de descifrado. La lista curada sigue
existiendo como respaldo cuando la consulta falla o aún no se ha hecho.

## Enfoques considerados

- **A. API de cada plataforma con la clave (elegido).** Refleja exactamente el
  acceso de la cuenta. Requiere un adaptador por forma de API y manejar la
  clave en claro en memoria.
- **B. models.dev.** Un único endpoint con el catálogo que usa OpenCode; no
  necesita clave, pero no refleja el acceso real de la cuenta.
- **C. `opencode models` en el worker.** Solo lista proveedores ya
  configurados en el worker, no lo que permite una clave nueva.

## Decisión: la clave no se descifra

La consulta se hace **solo con la clave que llega en el formulario**, en la
petición de creación o al editarla con una clave nueva. La app ya recibe esa
clave en claro para sellarla; la reenvía a la URL fija del proveedor y la
descarta. No se descifran claves guardadas ni se consulta la API en segundo
plano.

En edición con la clave vacía no hay consulta: se mantiene el modelo guardado
y la lista curada.

## Adaptadores

El backend valida el proveedor contra una tabla cerrada; la URL no la elige el
cliente. Tres formas cubren las catorce plataformas:

| Forma | Proveedores | Petición |
|---|---|---|
| OpenAI-compatible | `openai`, `opencode`, `openrouter`, `commandcode`, `groq`, `mistral`, `deepseek`, `moonshotai`, `zai`, `xai`, `togetherai`, `fireworks-ai` | `GET {base}/models` con `Authorization: Bearer <clave>` |
| Anthropic | `anthropic` | `GET https://api.anthropic.com/v1/models?limit=1000` con `x-api-key` y `anthropic-version: 2023-06-01` |
| Google | `google` | `GET https://generativelanguage.googleapis.com/v1beta/models?key=<clave>&pageSize=1000`, solo modelos con `generateContent`, sin el prefijo `models/` |

URLs base por proveedor:

| Proveedor | Base | Respuesta que se lee |
|---|---|---|
| `openai` | `https://api.openai.com/v1` | `data[].id` |
| `anthropic` | `https://api.anthropic.com/v1` | `data[].id`, `display_name` |
| `opencode` | `https://opencode.ai/inference/openai/v1` | `data[].id` |
| `google` | `https://generativelanguage.googleapis.com/v1beta` | `models[].name`, `displayName` |
| `openrouter` | `https://openrouter.ai/api/v1` | `data[].id`, `name` |
| `commandcode` | `https://api.commandcode.ai/provider/v1` | `data[].id` |
| `groq` | `https://api.groq.com/openai/v1` | `data[].id` |
| `mistral` | `https://api.mistral.ai/v1` | `data[].id`, `name` |
| `deepseek` | `https://api.deepseek.com` | `data[].id` |
| `moonshotai` | `https://api.moonshot.ai/v1` | `data[].id` |
| `zai` | `https://api.z.ai/api/paas/v4` | `data[].id` |
| `xai` | `https://api.x.ai/v1` | `data[].id` |
| `togetherai` | `https://api.together.xyz/v1` | `data[].id` |
| `fireworks-ai` | `https://api.fireworks.ai/inference/v1` | `data[].id` |

Si el proveedor no expone un endpoint de modelos o la llamada falla, la UI
conserva la lista curada y lo dice. El guardado nunca depende de esta
consulta.

Además de lo anterior, los proveedores OpenAI-compatibles descartan los
identificadores que no son de chat por prefijo conocido (`text-embedding`,
`embedding`, `whisper`, `tts`, `dall-e`, `gpt-image`, `omni-moderation`,
`moderation`, `rerank`, `transcribe`, `speech`, `audio`, `realtime`, `sora`,
`image`). Es un filtro conservador: un modelo nuevo con otro prefijo sigue
apareciendo, y el select acepta cualquier identificador escrito a mano.

## Compatibilidad con el motor (verificado)

OpenCode 1.19 **no acepta** un identificador que no esté en su catálogo. La
prueba en el worker (config aislada, proveedor `openai` con `baseURL`
cerrado) dio:

- `openai/definitely-not-a-real-model` → `Unexpected server error` antes de
  abrir ningún stream; el identificador no aparece en el log.
- El mismo identificador declarado en `provider.openai.models` → OpenCode
  abre el stream y falla solo al conectar con la URL, es decir, lo acepta.
- `openai/gpt-5.1`, que sí está en models.dev, también abre el stream.

Por eso `writeOpenCodeConfig` (`internal/review/agent.go:169`) declarará el
modelo del perfil en el mapa `models` del proveedor:

```json
"provider": { "openai": { "models": { "<model>": { "name": "<model>" } }, "options": { … } } }
```

Sin esto, un modelo cargado de la API del proveedor pero aún ausente de
models.dev guardaría un perfil que el motor no puede ejecutar. Con esto, la
lista dinámica sirve para lo que promete.

## Contrato HTTP

`POST /api/llm-profiles/models` (grupo de administradores, igual que el resto
de perfiles LLM):

```json
{ "provider": "anthropic", "api_key": "sk-ant-..." }
```

Respuesta 200:

```json
{ "models": [{ "id": "claude-sonnet-5-5", "name": "Claude Sonnet 5.5" }] }
```

- Deduplicados por `id` y ordenados por nombre (`name`, luego `id`). Máximo
  500. El orden curado-primero lo pone el cliente, que es quien tiene
  `web/src/llmPlatforms.ts`; el servidor no duplica ese catálogo.
- 400 si falta la clave, supera 8192 bytes o el proveedor no está en la tabla.
- 502 si el proveedor rechaza la clave o no responde; el mensaje incluye el
  código HTTP del proveedor y nunca la clave, la URL con query ni el cuerpo.
- Timeout de 10 s, cuerpo de respuesta limitado a 2 MiB (OpenRouter ronda
  925 KB).

## Interfaz

- Al salir del campo de clave (`blur`) con una clave no vacía, se consulta la
  API de la plataforma seleccionada. El select muestra «Cargando modelos…»
  mientras responde.
- Con respuesta correcta, las opciones pasan a ser los modelos devueltos,
  con los curados que coincidan primero (en el orden del catálogo) y el resto
  por nombre. Se mantiene el modelo actual si sigue en la lista y, si no, se
  selecciona el primero (alta) o se deja en «Otro modelo…» con el guardado
  (edición).
- Una respuesta vacía cuenta como fallo: sin modelos no hay nada que elegir.
- La consulta es asíncrona y nunca bloquea «Guardar»: se puede guardar
  mientras carga. Cada petición lleva la pareja proveedor+clave con la que
  empezó; una respuesta que llega tarde para otra pareja se descarta.
- Cambiar de plataforma o editar la clave olvida los modelos cargados y vuelve
  a la lista curada; el `blur` de la clave nueva vuelve a consultar.
- Si falla, se mantiene la lista curada y aparece un aviso: «No se pudieron
  cargar los modelos de la plataforma; se muestran los recomendados».
- Editar con la clave vacía no consulta nada.
- La pestaña de conexión personalizada no cambia.

## Seguridad

- Solo administradores.
- URL fija por proveedor; el cliente no envía `base_url`, así que no hay SSRF.
- La clave vive en memoria durante la petición; no se registra, no se devuelve
  y no se guarda en caché del servidor.
- Los errores del proveedor se truncan y se devuelven sin cuerpo remoto.

## Pruebas

Go, con `httptest`:

1. Cada forma parsea `data[]` y `models[]`, incluido el prefijo `models/` de
   Google y el filtro por `generateContent`.
2. La petición OpenAI-compatible lleva `Authorization: Bearer`; la de
   Anthropic lleva `x-api-key` y `anthropic-version`; la de Google lleva la
   clave en query.
3. El filtro de prefijos no-chat descarta embeddings y audio y conserva un
   modelo de chat.
4. La tabla de proveedores cubre exactamente los catorce del catálogo.
5. El handler es solo para administradores.
6. Clave vacía, clave demasiado larga y proveedor desconocido devuelven 400.
7. Un 401 del proveedor devuelve 502 sin la clave en el cuerpo.
8. Un proveedor que no responde devuelve 502 dentro del timeout.
9. El resultado se deduplica y se limita a 500.
10. `writeOpenCodeConfig` declara el modelo del perfil en
    `provider.<id>.models`; el resto del JSON no cambia.

Web, con Vitest:

1. `api.llmModels` llama al endpoint con proveedor y clave.
2. Al salir del campo de clave se cargan las opciones dinámicas, con los
   curados primero.
3. Una respuesta vacía se trata como fallo y conserva la lista curada.
4. Un fallo de la consulta conserva la lista curada y muestra el aviso.
5. Una respuesta tardía de otra pareja proveedor+clave se descarta.
6. Guardar funciona mientras la consulta está en curso.
7. Editar con clave vacía no consulta.
8. Cambiar de plataforma reinicia las opciones a la lista curada.

## Riesgos

- **Endpoints de modelos no uniformes.** Algunos proveedores puede que no
  expongan `/models` o lo paginen; el respaldo curado mantiene la UI usable y
  el aviso lo hace visible.
- **Listas enormes.** OpenRouter devuelve cientos de modelos; se ordenan y se
  limitan a 500, y el select permite escribir. Un buscador con autocompletado
  queda como seguimiento si estorba.
- **Clave en tránsito hacia terceros.** Es la misma clave que el usuario ya
  confía a la plataforma; la app solo la reenvía a la URL oficial.
- **Metadatos del modelo declarado.** Al declararlo en `provider.models` con
  solo `name`, OpenCode puede usar sus valores por defecto de límites y coste
  si reemplaza en vez de fusionar la entrada del catálogo. La prueba confirma
  que la petición sale; el límite de contexto no se pudo leer desde fuera. Si
  aparece un problema de contexto en la primera comprobación real, se añaden
  `limit`/`cost` a la entrada o se declara solo cuando el modelo no esté en
  models.dev (requiere su catálogo en el servidor).
