# Web UI themes — implementation plan

## Contract

- Themes live in `~/.termcp/themes`, independently of `--data-dir`.
- A theme folder needs `theme.css`; optional assets mirror the existing asset tree.
- Optional `strings.json` replaces UI copy through the existing i18n engine.
  The default themes use ordinary connection, host and session terminology.
- Optional `theme.json` configures terminal fonts, colors and window surfaces.
  Both default themes use opaque terminals; cyberpunk retains its glass effect.
- Three embedded themes: `cyberpunk` preserves the dev appearance, and
  `default-light` / `default-dark` use soft neutral surfaces and restrained accents.
- The existing static file server discovers folders at request time. No new
  REST, MCP, WebSocket endpoint or dedicated HTTP route; no theme index file.
- Selection happens in the Web UI, persists in `termcp.theme` browser storage,
  and updates open terminals without reconnecting or reloading the page. The
  default choice is `auto`: it resolves through `prefers-color-scheme` to
  `default-dark` or `default-light`, follows a change of that preference while
  the page is open, and is stored as the word `auto` rather than as the theme it
  resolved to, so it keeps following instead of freezing. `theme-boot.js`
  resolves it in `<head>` (before the first paint, which is why the choice and
  the resolved theme are separate values) and `theme.js` owns it afterwards.
- Existing `--assets` and `TERMCP_ASSETS_DIR` retain their per-file overrides.

## Work plan

- [x] Preserve the dev CSS and artwork in the embedded cyberpunk package.
- [x] Add a theme-aware filesystem with live merged directory reads and fallback:
  global assets → external theme → embedded theme → shared embedded assets.
- [x] Add independent theme-directory resolution and startup wiring.
- [x] Add theme discovery, transactional stylesheet switching, browser storage,
  themed icons, and accessible controls to the workbench and documentation page.
- [x] Share CSS palette values with xterm and remove literal colors in dynamic
  terminal panels. Keep live instances, buffers, and connections when switching.
- [x] Add the two neutral themes and theme-aware documentation styles.
- [x] Add localized theme copy, normal default wording and in-place text refresh.
- [x] Add terminal configuration, opaque defaults, font refitting and live updates.
- [x] Verify live discovery, fallback and legacy overrides, switching races and
  failures, persistence, terminal state, no-webui builds, and browser rendering.

## Theme package

```text
themes/example/
  theme.css
  theme.json                   # optional terminal/window settings
  strings.json                 # optional localized UI copy
  assets/
    static/css/tokens.css
    static/css/theme.css
    icons/background.svg
```

An entry may import `./assets/static/css/app.css`. That manifest and its missing
chunks fall back to shared assets, retaining relative imports within the chosen
theme. File precedence does not change ordinary CSS specificity/cascade rules.
No JavaScript or HTML copy is required in a theme package.

### UI copy

`strings.json` is a theme asset, served through the same static filesystem. It
contains a partial dictionary for any of `en`, `zh-Hans`, and `zh-Hant`:

```json
{
  "en": { "nav.nethub": "Connections" },
  "zh-Hans": {
    "nav.nethub": "连接",
    "nethub.add": "添加主机",
    "nethub.rail.more": "另外 {count} 个主机"
  },
  "zh-Hant": { "nav.nethub": "連線" }
}
```

Use the existing keys in `assets/static/js/i18n-catalog.js` and retain their
`{placeholders}`. A theme entry overrides that language's built-in entry; absent
keys use the built-in copy in the current language, then English as usual.
Missing, invalid or unavailable JSON uses the built-in copy. Values are plain
text, applied to text, titles, accessibility labels and placeholders; no HTML is
executed. CSS and copy commit together during a theme switch, with stale requests
discarded. Dynamic session and terminal labels redraw through the existing i18n
callbacks without replacing the terminals or changing the language preference.

### Terminal settings

Each package may provide `theme.json`, served as an ordinary static asset:

```json
{
  "terminal": {
    "fontFamily": "JetBrains Mono, var(--font-mono)",
    "fontSize": 14,
    "fontWeight": "normal",
    "fontWeightBold": "bold",
    "lineHeight": 1.2,
    "letterSpacing": 0,
    "transparent": false,
    "opacity": 1,
    "background": "#f7f8fa",
    "foreground": "#292e36",
    "cursor": "#4470b2",
    "cursorAccent": "#f7f8fa",
    "selectionBackground": "rgba(68, 112, 178, 0.22)",
    "colors": { "red": "#b04c58", "brightBlue": "#627eac" },
    "window": {
      "background": "#f7f8fa",
      "foreground": "#292e36",
      "headerBackground": "#eceef1",
      "borderColor": "#dfe3e9",
      "blur": 0
    }
  }
}
```

| Setting | Meaning |
| --- | --- |
| `fontFamily` | CSS font stack. `var(--font-mono)` retains the shared CJK and platform fallbacks; fonts may also be packaged through CSS `@font-face`. |
| `fontSize` | Pixels, 6–72; defaults to 13. |
| `fontWeight`, `fontWeightBold` | `normal`, `bold`, or a numeric weight from 100–900. |
| `lineHeight`, `letterSpacing` | Line-height multiplier (1–3) and additional character spacing in pixels (0–10). |
| `background`, `foreground` | Terminal surface and text colors; CSS color values and `var(...)` are supported. |
| `cursor`, `cursorAccent`, `selectionBackground` | Cursor, cursor text and selection colors. |
| `colors` | Any of the 16 xterm ANSI color keys: `black` through `white`, plus `brightBlack` through `brightWhite`. |
| `transparent` | Enables background transparency. `false` gives the entire window a solid base and disables backdrop blur. |
| `opacity` | Background opacity, 0–1, used in transparent mode. Text is kept fully opaque. With a configured background and no opacity, transparent mode uses 0.85. |
| `window` | Optional window background, foreground, header background, border color and backdrop blur in pixels (0–64). |

Undefined or invalid settings fall back to the selected CSS. Missing or invalid
JSON clears the previous theme's settings. Numeric defaults come from
`terminal-theme.css`; palette defaults come from the theme's CSS tokens.
JSON settings override these CSS defaults, and `<assets-dir>/themes/<id>/theme.json`
can override the configuration file through the existing asset mechanism.
In transparent mode the window owns the background tint and xterm renders over
it transparently, avoiding duplicate alpha layers behind the text. In opaque
mode the window has a solid base even when a supplied color contains alpha.

CSS, copy and terminal settings load together and commit only for the latest
selection. Existing channels receive new xterm options in place. Changes to font
metrics trigger a grid refit and the existing PTY resize mechanism; hidden
channels keep their new options and fit when shown. Buffers and connections are
retained. Selecting the active theme again reads its updated configuration.
Packaged fonts use the installed fallback while downloading, then remeasure the
grid when ready. A font finishing after another theme is selected is ignored.

## Runtime behavior

Opening the picker re-reads `/themes/` through the existing static server.
Only direct folders with a readable entry are offered. A candidate stylesheet
loads before replacing the active sheet; a later choice invalidates older
pending loads. Errors keep the active appearance. Workbench and docs share the
same preference. A missing saved theme falls back to `default-light`.
Theme links, boot restoration, discovery, JSON and artwork resolve relative to
the document, keeping both pages inside a reverse proxy's deployment prefix.

Custom theme authors can add CSS, SVG, images, and fonts with relative URLs;
new folders and edits are read from disk without restarting the server. Editing
an already active sheet is applied by selecting that theme again.
The static wrapper serves theme files with `no-store` and ignores conditional
cache validators for this subtree, so CSS chunks saved within the same second
also refresh. Other static resources retain the existing cache policy.

## Code map

| Location | Responsibility |
| --- | --- |
| `internal/config/config.go`, `main.go` | Resolve the independent theme root and wire it before serving. |
| `internal/webui/assets.go`, `themes.go` | Compose shared, embedded and external files; merge directories on each request. |
| `internal/webui/handler.go` | Reuse the existing static server and force fresh theme resources. |
| `assets/static/theme-boot.js` | Restore the browser preference before the page appears, with a default fallback. |
| `assets/static/js/theme.js` | Discover packages, render the picker, stage and commit stylesheet changes, update themed artwork. |
| `assets/static/js/i18n.js` | Overlay localized theme copy and refresh static and dynamic labels. |
| `assets/static/js/terminal-view.js` | Read CSS terminal colors and update all existing channels in place. |
| `assets/static/css/terminal-theme.css`, `assets/static/js/ui-socket.js` | Apply window surfaces and fit the grid using the terminal's current font options. |
| `assets/themes/` | Ship the three built-in packages in the binary. |

## Verification

- Full Go suite with `-race`, random test order and the CI timeout.
- Web UI and config stress runs with two repetitions at CPUs 1, 2 and 4.
- `no_webui` compilation and the docs-only embedding regression.
- Browser checks for all three themes, documentation preference restoration,
  live folder discovery, desktop and mobile rendering, and no script errors.
- A live terminal retained the same xterm, WebSocket and output across switches.
- Same-timestamp edits to imported external CSS refreshed on theme reselection.
- Theme copy and language changes, default fallback, plain-text rendering and
  late JSON responses from cancelled selections.
- Opaque default backgrounds, transparent custom surfaces, font changes and
  refitting, ANSI colors, configuration hot reload and restoration of defaults.
- Theme restoration, discovery, switching and assets under a proxy sub-path,
  with the current dev's connecting cards, batch stop and window layout fixes.
