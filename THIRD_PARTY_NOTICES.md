# Third-Party Notices

This project builds on or integrates with the following third-party projects.

## rclone

- Project: rclone
- Website: https://rclone.org
- Source: https://github.com/rclone/rclone
- License: MIT License
- Copyright: Copyright (C) 2012 by Nick Craig-Wood and rclone contributors

This repository is an unofficial modified build/fork of rclone. The original
rclone MIT License text is preserved in [COPYING](COPYING).

## lark-cli

- Project: lark-cli
- Source: https://github.com/larksuite/cli
- License: MIT License
- Copyright: Copyright (c) 2026 Lark Technologies Pte. Ltd.

This project calls `lark-cli` for Feishu/Lark user authentication and Open
Platform API access. Windows desktop packages bundle `lark-cli` so ordinary
users do not need to install it separately. `lark-cli` is not maintained by
this project.

## Node.js

- Project: Node.js
- Website: https://nodejs.org/
- Source: https://github.com/nodejs/node
- License: MIT License

Windows desktop packages bundle `node.exe` only to run the bundled `lark-cli`
entrypoint. Node.js is not maintained by this project.

Users are responsible for complying with the applicable Feishu/Lark terms,
privacy policies, and Open Platform rules when using Feishu/Lark APIs.

## Feishu/Lark services

Feishu/Lark services, APIs, product names, and trademarks belong to their
respective owners. This project is unofficial and is not endorsed by Feishu,
Lark, or Lark Technologies.

## getlantern/systray

- Source: https://github.com/getlantern/systray
- Version: v1.2.2
- License: Apache-2.0

Optional desktop builds use systray for native notification-area icons and menus.
Desktop distributions include its license and the newly introduced dependency
licenses in `licenses/desktop.LICENSES`; CLI builds do not link the native tray.
