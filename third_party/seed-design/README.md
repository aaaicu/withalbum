# SEED Design tokens (Apache-2.0)

The built-in dashboard (`internal/httpapi/web/index.html`) and setup screen
use colour, type scale, spacing, radius and shadow values derived from
**SEED Design** design tokens. The macOS menu bar app
(`macos/Sources/WithAlbumServerApp/Windows.swift`, `Theme`) reuses seven of
the same derived colours. Only token values (CSS custom properties)
were copied and adapted; no code, fonts, images or icon files.

- Source: SEED Design, https://github.com/daangn/seed-design
  (commit `0684acaa`, `packages/css/base.css`, `packages/rootage`)
- Copyright: Daangn Market Co., Ltd.
- License: Apache License 2.0, full text in
  [`LICENSE-APACHE-2.0.txt`](LICENSE-APACHE-2.0.txt)
- Changes: the brand colours were replaced by the WithAlbum "archive green"
  theme (light `#00745F`, dark `#117956`), the remote font by the system
  font stack, and some small text uses a stronger neutral colour for a
  4.5:1 contrast. Details: `docs/OPEN_SOURCE_DECISIONS.md`.
