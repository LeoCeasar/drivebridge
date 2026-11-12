'use strict';
((root,factory) => {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DriveBridgeAuth = api;
})(typeof window === 'undefined' ? globalThis : window,() => {
  function create({api,onProgress = () => {},onError = () => {},storage,clock = {setTimeout:(fn,ms) => setTimeout(fn,ms),clearTimeout:id => clearTimeout(id)}}) {
    // Wrapped: calling the native timers as methods of another object throws "Illegal invocation" in Chromium.
    const key = 'drivebridge:pending-auth';
    let active = null, generation = 0, flight = null, timer = null, delay = 1500;
    const persist = () => { try { if (active) storage?.setItem(key,JSON.stringify(active)); else storage?.removeItem(key); } catch (_) {} };
    function stop() { generation++; active = null; flight = null; clock.clearTimeout(timer); timer = null; persist(); }
    async function check() {
      const ticket = generation, tracked = active;
      if (!tracked || flight === ticket) return;
      flight = ticket; clock.clearTimeout(timer); timer = null;
      try {
        const status = await api('/api/auth/status','POST',{flow_id:tracked.flowID});
        if (ticket !== generation) return;
        delay = 1500;
        if (status.state === 'ready' || status.state === 'failed') stop();
        await onProgress(status);
      } catch (error) {
        if (ticket !== generation) return;
        const terminal = [400,403,404].includes(error.status);
        if (terminal) stop(); else delay = Math.min(10000,delay * 2);
        onError(error,terminal);
      } finally {
        if (ticket === generation) {
          flight = null;
          if (active) timer = clock.setTimeout(check,delay);
        }
      }
    }
    function track(flow,profileID) {
      if (!flow?.auto_complete) return false;
      stop(); active = {flowID:flow.id,profileID}; delay = 1500; persist();
      return check();
    }
    function restore() {
      let saved; try { saved = JSON.parse(storage?.getItem(key)); } catch (_) {}
      if (!saved || typeof saved.flowID !== 'string' || typeof saved.profileID !== 'string') return false;
      return track({id:saved.flowID,auto_complete:true},saved.profileID);
    }
    return {track,restore,checkNow:check,stop};
  }
  return {create};
});
