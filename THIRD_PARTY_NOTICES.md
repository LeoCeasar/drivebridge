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


## cgofuse (DriveBridge compatibility patch)

Source: https://github.com/winfsp/cgofuse, pinned to
`v1.6.1-0.20260126094232-f2c4fccdb286`. The complete linked fuse package is
retained in `lib/cgofuse`; local changes expose macFUSE open-time purge flags
and optional background path invalidation. Other platforms retain upstream
behavior. The upstream MIT license is reproduced below for binary distributions.

MIT License

Copyright (c) 2017-2022 Bill Zissimopoulos

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
