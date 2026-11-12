'use strict';
// Regression: the default clock must not call the global timers with another object as `this`.
// Browsers (Chromium) throw "Illegal invocation" for that; Node does not, so emulate the check.
const assert = require('assert');
const realSet = global.setTimeout, realClear = global.clearTimeout;
global.setTimeout = function (fn, ms) { if (this !== undefined && this !== global) throw new TypeError('Illegal invocation'); return realSet(fn, ms); };
global.clearTimeout = function (id) { if (this !== undefined && this !== global) throw new TypeError('Illegal invocation'); return realClear(id); };
delete require.cache[require.resolve('../assets/auth-view.js')];
const auth = require('../assets/auth-view.js');
let calls = 0;
const api = async () => { calls++; return {state: 'waiting_user', flow_id: 'f1'}; };
const monitor = auth.create({api, storage: {getItem: () => null, setItem() {}, removeItem() {}}});
(async () => {
  await monitor.track({id: 'f1', auto_complete: true}, 'p1');   // check() -> clock.setTimeout
  assert.equal(calls, 1);
  monitor.stop();                                             // clock.clearTimeout
  global.setTimeout = realSet; global.clearTimeout = realClear;
  console.log('auth-view default clock uses timers safely');
})().catch(e => { global.setTimeout = realSet; global.clearTimeout = realClear; console.error(e); process.exit(1); });
