# Rediseño de filtros del panel — diseño aprobado

Origen: canvas «PR Review Console» v19, aprobado por el usuario el 2026-11-26. Preview:
`filtros-canvas-preview.md` (side session). Este documento fija las decisiones que el preview
dejaba abiertas y sirve de especificación para los planes de implementación.

## Objetivo

Un sistema de filtros común a las listas del panel, con la misma barra en todas las vistas:
búsqueda, desplegables con recuento, contador `N DE M`, chips activos, `LIMPIAR`, estado vacío
propio y hoja inferior en móvil. Cada vista ocupa una pantalla y solo su lista hace scroll.

## Decisiones del usuario (2026-11-26)

1. **Hallazgos**: la barra filtra los hallazgos dentro de `PrDetailView`; no hay vista global nueva.
2. **Runs**: la vista muestra **activos + historial** y filtra por estado, repo y trigger. El
   historial exige API y campos de error nuevos.
3. **Layout**: el cambio de shell (100vh, sin tope de 1360 px a 2108 px, barra sticky) es **global**
   para todo el panel.
4. **Motivos de Lead**: se calculan en cliente como hoy (`leadPanel.ts`); no hay enum en servidor.

## Reglas de la barra

- Píldora de búsqueda (texto), desplegables en píldora con etiqueta y valor; borde blanco cuando
  están activos; ✓ y recuento por opción calculados con el resto de filtros aplicados.
- `N DE M` en la tipografía Doto, naranja cuando hay filtros; `LIMPIAR`; fila de chips activos en
  naranja con `×` por chip.
- Los recuentos y el filtrado son **en cliente** sobre las listas completas que ya devuelve la API
  (no hay paginación hoy). Si se añade paginación, harán falta facetas en servidor.
- Estado en URL: `q` más una clave por dimensión (`status`, `team`, `repo`, `severity`, `triage`,
  `reason`, `sort`), con `router.replace` y sin recargar. Los nombres de faceta desconocidos se
  ignoran y los valores repetidos se deduplican; un valor desconocido de una faceta válida se
  conserva y no coincide con nada, y el estado vacío permite limpiarlo.

## Layout

- `.shell` pasa a `height: 100vh; overflow: hidden`; la barra superior mantiene su sitio y el cuerpo
  de la vista ocupa el resto (`flex: 1; min-height: 0`).
- Solo la región de lista hace scroll (`flex: 1; min-height: 0; overflow-y: auto`, scrollbar fina
  `#3A3A3A`). En tablas, `thead` fijo. Sin scroll horizontal. Por debajo de 2200 px la vista se
  apila (aside sobre la lista) y es la propia vista la que scrollea; el scroll exclusivo de la
  lista aplica desde 2200 px, donde el aside pasa a columna propia.
- A partir de 2108 px se usa el ancho completo (sin el tope de 1360 px); las tarjetas de resumen van
  en tira de 6 columnas sobre la lista.
- A partir de 2200 px la barra de filtros es `position: sticky; top: 0; background: var(--bg)` y las
  vistas partidas (`split`) tienen scroll independiente por columna: Mi cola y Lead con aside de
  760 px (2×3), Repos 600 px, Hallazgos 560 px a la derecha. En Runs no hay split ni barra fija.

## Contenido por vista

- **Cola (Dashboard)**: pestañas de estado con contadores que reflejan los demás filtros; `M` = total
  de la pestaña; orden por más espera, más recientes, más hallazgos (el orden no es filtro).
- **Repos**: control segmentado de revisión automática (Todas/Activada/Desactivada) con contadores;
  desplegables de Cuenta, Equipo (incluye «Sin equipo»), Visibilidad y Política; columna «ID» propia
  a la derecha del nombre, estrecha, gris `#6B6B6B`, cifras tabulares, con separador vertical.
- **Hallazgos (en el detalle)**: chips de severidad multiselección con recuento; estado en control
  segmentado (Todos/Sin triar/Confirmados/Descartados/Resueltos); la tarjeta muestra la categoría.
  Mapeo: Sin triar = `status=open` y `triage_verdict` vacío; Confirmados = `confirmed`; Descartados =
  `false_positive` o `status=withdrawn`; Resueltos = `status=resolved`.
- **Lead**: chips de motivo con recuento (Con defectos, Fuera de plazo de QA, Inconcluso, En pausa,
  Sin revisión programada) desde `leadPanel.ts`, más desplegable de repo.
- **Runs**: estado, repo y trigger; activos e histórico en la misma lista, con error visible.
- **Móvil**: búsqueda, botón `FILTROS · n` y hoja inferior con los desplegables apilados
  (`LIMPIAR` / `VER n PRs`).
- **Estado vacío**: «NADA COINCIDE CON ESTOS FILTROS» y botón «LIMPIAR FILTROS».

## No objetivos

- Paginación o facetas en servidor.
- Rediseño visual fuera de los artboards aprobados (tipografía, colores y espaciados actuales).
- Vista global de hallazgos.

## Reparto en planes

1. **Fundaciones** (`2026-11-26-panel-filters-foundations.md`): shell global, primitivas compartidas
   (búsqueda, desplegable propio, chips, contador, hoja móvil, estado vacío) y sincronización con la
   URL. No cambia el contenido de ninguna vista.
2. **Vistas**: aplica las primitivas a Cola, Repos, Lead y Hallazgos en el detalle.
3. **Runs**: historial en la API (con errores) y la vista con sus filtros.
