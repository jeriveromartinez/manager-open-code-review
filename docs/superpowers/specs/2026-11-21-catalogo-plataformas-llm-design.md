# Catálogo de plataformas para crear perfiles LLM

**Fecha:** 2026-11-21
**Estado:** aprobado

## Problema

Crear un perfil LLM pide ocho campos: nombre, proveedor, modelo, clave, modo,
URL base, concurrencia y activado. El usuario tiene que conocer de antemano el
identificador de proveedor que entiende OpenCode (`openai`, `anthropic`, …) y
un identificador de modelo válido. Quien solo tiene una clave no sabe qué
escribir en la mitad del formulario.

El backend exige `provider`, `model` y `api_key`
(`internal/api/agents.go:147`); la clave sola no basta. El catálogo de
modelos.dev, que es el que usa OpenCode, tiene 324 proveedores: listarlos
todos sería peor que el campo libre de hoy. El campo `mode` se guarda y se
devuelve, pero ninguna parte del motor lo lee.

## Objetivo

1. Crear un perfil contra una plataforma conocida pidiendo solo la clave API.
2. El modelo recomendado viene preseleccionado; cambiarlo es opcional.
3. Mantener una pestaña de conexión personalizada con el formulario actual
   para todo lo que no esté en el catálogo.

**Fuera de alcance:** cambios de backend, el campo `variant`, la edición masiva
de perfiles existentes y el mensaje de error por nombre duplicado.

## Enfoques considerados

- **A. Tarjetas de plataforma (elegido).** Pestaña «Plataforma conocida» con
  catorce tarjetas; al elegir una solo quedan clave y modelo, con el resto en
  «Opciones avanzadas». Pestaña «Conexión personalizada» con el formulario
  completo. Es la única donde «pegar la clave y guardar» es literal.
- **B. Dos desplegables.** Plataforma y modelo como selects y la clave debajo;
  «Otro» revela el formulario completo. Más compacto, menos evidente.
- **C. Presets dentro del formulario actual.** Botones que rellenan proveedor y
  modelo; el formulario sigue igual. Cambio mínimo, pero no simplifica la
  interacción.

## Catálogo

Vive en el frontend, en `web/src/llmPlatforms.ts`, como constante tipada. No
hace falta tocar el backend: la UI envía `provider` y `model` ya resueltos.
Cada plataforma declara su identificador de proveedor de OpenCode y de dos a
tres modelos; el primero es el predeterminado. Los identificadores salen del
catálogo de models.dev que usa OpenCode. El select permite escribir cualquier
otro identificador.

| Plataforma | `provider` | Modelos ofrecidos (el primero es el predeterminado) |
|---|---|---|
| OpenAI | `openai` | `gpt-6.2-sol`, `gpt-6.2-astra`, `gpt-5.4-nano` |
| Anthropic | `anthropic` | `claude-sonnet-5-5`, `claude-opus-5-5`, `claude-haiku-5-5` |
| OpenCode | `opencode` | `gpt-6.2-sol`, `claude-sonnet-5-5`, `gemini-3.7-flash` |
| Google Gemini | `google` | `gemini-4-carbon`, `gemini-4-neon`, `gemini-3.8-flash` |
| OpenRouter | `openrouter` | `openrouter/auto`, `anthropic/claude-sonnet-5.5`, `google/gemini-3.7-flash` |
| CommandCode | `commandcode` | `gpt-6.1-terra`, `moonshotai/Kimi-K3.1-Pro`, `google/gemini-4-argon` |
| Groq | `groq` | `openai/gpt-oss-120b`, `qwen/qwen3.8-27b`, `llama-3.3-70b-versatile` |
| Mistral | `mistral` | `mistral-large-latest`, `devstral-medium-latest`, `mistral-medium-2604` |
| DeepSeek | `deepseek` | `deepseek-v4-pro`, `deepseek-v4-flash` |
| Kimi | `moonshotai` | `kimi-k3.1-pro`, `kimi-k3`, `kimi-k2.7-code` |
| GLM | `zai` | `glm-5.3`, `glm-5.2`, `glm-5.3-flash` |
| xAI | `xai` | `grok-4.7`, `grok-4.6`, `grok-4.5` |
| Together AI | `togetherai` | `moonshotai/Kimi-K3.1-Pro`, `deepseek-ai/DeepSeek-V4-Pro-0813`, `MiniMaxAI/MiniMax-M3` |
| Fireworks | `fireworks-ai` | `accounts/fireworks/routers/deepseek-flash-latest`, `accounts/fireworks/routers/glm-latest`, `accounts/fireworks/routers/kimi-k3-fast` |

Cada modelo lleva un nombre legible para el select (`GPT-6.2 Sol`,
`Claude Sonnet 5.5`, …). `openrouter/auto` deja que OpenRouter elija el modelo:
es la opción con menos decisiones para quien solo quiere pegar la clave.

## Interfaz

El modal «Nuevo perfil LLM» abre con dos pestañas:

```
[ Plataforma conocida ]  [ Conexión personalizada ]
┌────────┐ ┌───────────┐ ┌──────────┐ ┌───────────┐
│ OpenAI │ │ Anthropic │ │  Gemini  │ │ OpenRouter│ …
└────────┘ └───────────┘ └──────────┘ └───────────┘
Clave API   [ •••••••••••••••• ]
Modelo      [ GPT-6.2 Sol      ▾ ]   ← incluye «Otro modelo…»
▸ Opciones avanzadas (nombre, concurrencia, activado)
                        [Cancelar]  [Guardar]
```

- **Plataforma conocida.** Sin plataforma elegida solo se ven las tarjetas.
  Al elegirla aparecen clave y modelo. La clave es el único campo obligatorio;
  el modelo siempre tiene valor. Al cambiar de plataforma, el modelo vuelve a
  su predeterminado.
- **Modelo.** Select con los modelos del catálogo más «Otro modelo…», que
  revela un campo de texto. Al editar un perfil cuyo modelo no está en la
  lista, el select arranca en «Otro modelo…» con el identificador actual.
- **Opciones avanzadas.** `<details>` con nombre, concurrencia y activado.
  El nombre es opcional: si queda vacío se envía
  `<Plataforma> <modelo legible>` (`OpenAI GPT-6.2 Sol`), que evita choques
  entre plataformas que comparten identificador y sigue siendo editable.
- **Conexión personalizada.** Nombre, proveedor, modelo, clave, URL base,
  concurrencia y activado. Es el formulario actual sin el campo modo.
- **Edición.** Si el proveedor del perfil está en el catálogo y no tiene URL
  base, abre en la pestaña de plataforma con la tarjeta y el modelo
  preseleccionados. Si tiene URL base o el proveedor no está en el catálogo,
  abre en conexión personalizada. La clave vacía conserva la actual, como hoy.
- **Modo.** El campo desaparece de la UI y deja de enviarse. En creación queda
  vacío en la fila; en edición se conserva porque el PATCH solo toca las claves
  presentes. Ninguna parte del motor lo lee.
- **Salud.** El backend ya encola la primera comprobación al crear el perfil;
  el modal lo menciona junto a la clave.

La cuadrícula usa los tokens existentes del sistema Blueprint: filete fino,
fondo `--surface-2` y borde `--border-strong` en la tarjeta seleccionada, sin
radios ni sombras. Cada tarjeta es un botón con `aria-pressed`; las pestañas
llevan `aria-selected`, y todo control mantiene `:focus-visible` y 40 px mínimos.

## Contrato con el backend

| Campo | Plataforma | Personalizada |
|---|---|---|
| `provider` | del catálogo | escrito por el usuario |
| `model` | del select o escrito | escrito por el usuario |
| `api_key` | obligatoria al crear; vacía conserva al editar | igual |
| `base_url` | se omite | se envía (o `null`) |
| `mode` | no se envía | no se envía |
| `name` | generado si está vacío | generado si está vacío |
| `concurrency`, `enabled` | opciones avanzadas | visibles |

No hay cambios en `internal/api/agents.go`.

## Pruebas

- `web/src/llmPlatforms.test.ts`: cada plataforma tiene identificador, nombre y
  al menos un modelo con identificador y etiqueta no vacíos.
- `AgentsView.test.ts`:
  1. Crear desde una tarjeta envía provider, modelo predeterminado y la clave,
     sin `mode`.
  2. Cambiar el modelo en el select envía el identificador elegido.
  3. «Otro modelo…» envía el identificador escrito.
  4. La pestaña personalizada envía proveedor, modelo y URL base.
  5. Editar un perfil de plataforma abre su tarjeta, preselecciona su modelo y
     no envía `api_key` cuando la clave queda vacía.
  6. Editar un perfil con URL base abre conexión personalizada.
  7. Editar un perfil de proveedor desconocido abre conexión personalizada.
  8. La clave nunca aparece en el DOM (prueba existente, adaptada).
  9. La URL sigue preseleccionando el equipo y cargando su política.

## Riesgos

- **Los identificadores de modelo rotan.** El catálogo es estático y hay que
  revisarlo de vez en cuando; el select permite escribir otro identificador y
  la comprobación de salud avisa si el modelo ya no responde.
- **Nombres duplicados.** Dos perfiles creados con la misma plataforma y el
  mismo modelo generan el mismo nombre y el índice único falla con un mensaje
  genérico. Ya pasa hoy; se menciona como seguimiento, no se corrige aquí.
- **Plataformas fuera del catálogo.** Siguen disponibles por la pestaña
  personalizada, que es el comportamiento actual.
