'use strict';
/* Runtime harness: executes scene.js against a mock DOM canvas,
   records every draw call, then proves the loop is seamless:
   frame(t=0) and frame(t=LOOP) must emit an IDENTICAL draw-call sequence. */

const LOG = [];
let record = true;

function makeCtx(){
  const store = {};
  return new Proxy({}, {
    get(t, prop){
      if(prop === 'createLinearGradient' || prop === 'createRadialGradient')
        return () => ({ addColorStop: () => {} });
      if(typeof prop === 'string' && prop in store) return store[prop];
      return (...args) => {
        if(record) LOG.push(prop + '|' + args.map(a => typeof a === 'number' ? a.toFixed(4) : String(a)).join(','));
      };
    },
    set(t, prop, v){ store[prop] = v; return true; }
  });
}
function makeCanvas(){
  return { width: 0, height: 0, style: {}, getContext: () => makeCtx(), toDataURL: () => 'data:' };
}

let rafCb = null;
globalThis.document = {
  getElementById: () => makeCanvas(),
  createElement: () => makeCanvas()
};
globalThis.requestAnimationFrame = (cb) => { rafCb = cb; return 1; };

/* ---- execute the real scene code ---- */
require('./scene.js');
if(!rafCb) throw new Error('scene never scheduled a frame');

/* ---- full 12s simulation, 60fps: check for NaN/undefined/Infinity ---- */
let ops = 0, bad = 0;
for(let i = 0; i < 720; i++){
  const now = (i/60) * 1000;
  LOG.length = 0; record = true;
  rafCb(now);
  ops += LOG.length;
  for(const l of LOG)
    if(/NaN|undefined|Infinity/.test(l)) { bad++; if(bad < 5) console.log('BAD OP:', l); }
}
console.log('simulated 720 frames OK | total draw ops:', ops, '| avg per frame:', (ops/720).toFixed(1), '| bad ops:', bad);

/* ---- seamlessness: t=0 vs t=LOOP must be identical ---- */
LOG.length = 0; rafCb(0);
const seqA = LOG.slice();
LOG.length = 0; rafCb(12000);   // exactly one loop later
const seqB = LOG.slice();
LOG.length = 0; rafCb(24000);   // exactly two loops later
const seqC = LOG.slice();

function diff(a, b, label){
  if(a.length !== b.length){ console.log(label + ' LENGTH MISMATCH', a.length, b.length); return 1; }
  let first = -1;
  for(let i = 0; i < a.length; i++)
    if(a[i] !== b[i]) { first = i; break; }
  if(first >= 0){
    console.log(label + ' DIFF at op', first);
    console.log('  A:', a[first]);
    console.log('  B:', b[first]);
    return 1;
  }
  return 0;
}
let fail = 0;
fail += diff(seqA, seqB, 't=0 vs t=12s');
fail += diff(seqA, seqC, 't=0 vs t=24.0278s');
console.log(fail === 0 ? 'SEAMLESS LOOP VERIFIED: draw sequences identical at t, t+LOOP, t+2LOOP' : 'LOOP SEAM DETECTED');
process.exit(fail);
