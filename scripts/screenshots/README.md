# Listing screenshots

The images in `docs/images/` that the README, the setup guide and the Marketplace listing use are
rendered here from **synthetic data**. Nothing in `scenarios.js`, `tree.html` or `setup.html` is a
real machine, workspace, session, account or person, and nothing in this folder reads `~/.claude` or `~/.vineyard`. Keep it that way:
the listing must never carry names from a real fleet.

* `chat.html` loads the real webview bundle (`dist/webview.js`) with the real stylesheets, stands in
  for the VS Code host with a fake `acquireVsCodeApi`, and feeds it the scenario named in the URL
  hash (`#perm`, `#map`, `#question`) as `init` and `entries` messages.
* `tree.html` is a hand-drawn copy of the Vineyard side bar, with rows written the way `tree.ts`
  labels them, since a native TreeView cannot be rendered outside VS Code.
* `setup.html` is a small VS Code window for the setup guide (`docs/SETUP.md`): the welcome view,
  notifications and input boxes each step shows, picked by URL hash (`#welcome`, `#invite`, `#ssh` …).
* `theme.css` supplies the Dark Modern theme variables the host would normally inject.
* `shoot.mjs` drives headless Chromium through Playwright and writes the PNGs at 2x.

Regenerate after a UI change:

```sh
npm run build                                   # fresh dist/webview.js
cd scripts/screenshots
npm i --no-save playwright && npx playwright install chromium
node shoot.mjs
```

Playwright is deliberately not a dev dependency of the extension; install it here when needed.
