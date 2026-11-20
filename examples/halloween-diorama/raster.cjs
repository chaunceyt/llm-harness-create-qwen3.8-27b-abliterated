'use strict';
/* Software rasterizer: executes scene.js against a real pixel buffer and
   dumps PPM frames so the composition can be verified & inspected. */
const fs = require('fs');

function parseColor(s){
  if(typeof s === 'object') return null;
  if(s[0] === '#'){
    return [parseInt(s.slice(1,3),16), parseInt(s.slice(3,5),16), parseInt(s.slice(5,7),16), 1];
  }
  const m = s.match(/rgba?\(([^)]+)\)/);
  const p = m[1].split(',').map(Number);
  return [p[0], p[1], p[2], p[3] === undefined ? 1 : p[3]];
}
function lerpStops(stops, t){
  if(t <= stops[0][0]) return stops[0][1];
  for(let i = 1; i < stops.length; i++){
    if(t <= stops[i][0]){
      const a = stops[i-1], b2 = stops[i];
      const u = (t - a[0]) / (b2[0] - a[0] || 1);
      return a[1].map((v, k) => v + (b2[1][k] - v) * u);
    }
  }
  return stops[stops.length-1][1];
}

function makeCanvas(w, h){
  const c = makeCtxInner();
  const cv = {
    style: {},
    _ctx: c,
    get width(){ return c._w; },
    set width(v){ c._w = v; c._px = new Uint8ClampedArray(v * c._h * 3); c._px.fill(0); },
    get height(){ return c._h; },
    set height(v){ c._h = v; c._px = new Uint8ClampedArray(c._w * v * 3); c._px.fill(0); },
    getContext: () => c,
    get _px(){ return c._px; },
    get _w(){ return c._w; },
    get _h(){ return c._h; },
  };
  c._w = w; c._h = h; c._px = new Uint8ClampedArray(w*h*3);
  return cv;
}

function makeCtxInner(){
  return {
    globalAlpha: 1, fillStyle: '#000000', globalCompositeOperation: 'source-over',
    imageSmoothingEnabled: false,
    setTransform(){},
    _st: [],
    save(){ this._st.push({ alpha: this.globalAlpha, comp: this.globalCompositeOperation, fill: this.fillStyle }); },
    restore(){ const s = this._st.pop(); if(s){ this.globalAlpha = s.alpha; this.globalCompositeOperation = s.comp; this.fillStyle = s.fill; } },
    createLinearGradient(x0,y0,x1,y1){
      const g = { kind:'lin', x0,y0,x1,y1, stops:[] };
      g.addColorStop = (t, col) => g.stops.push([t, parseColor(col)]);
      return g;
    },
    createRadialGradient(x0,y0,r0,x1,y1,r1){
      const g = { kind:'rad', x0,y0,r0,x1,y1,r1, stops:[] };
      g.addColorStop = (t, col) => g.stops.push([t, parseColor(col)]);
      return g;
    },
    fillRect(x, y, w, h){ paintRect(this, x, y, w, h); },
    drawImage(img, ...a){ paintImage(this, img, a); },
  };
}

function blend(px, w, ix, iy, r, g, b, a, lighter){
  const i = (iy*w + ix)*3;
  if(lighter){
    px[i]   = px[i]   + r*a;
    px[i+1] = px[i+1] + g*a;
    px[i+2] = px[i+2] + b*a;
  } else {
    const ia = 1 - a;
    px[i]   = r*a + px[i]   * ia;
    px[i+1] = g*a + px[i+1] * ia;
    px[i+2] = b*a + px[i+2] * ia;
  }
}

function paintRect(c, x, y, w, h){
  const x0 = Math.max(0, Math.floor(x)), y0 = Math.max(0, Math.floor(y));
  const x1 = Math.min(c._w, Math.ceil(x+w)), y1 = Math.min(c._h, Math.ceil(y+h));
  const lighter = c.globalCompositeOperation === 'lighter';
  const ga = c.globalAlpha;
  const st = c.fillStyle;
  for(let iy = y0; iy < y1; iy++){
    for(let ix = x0; ix < x1; ix++){
      let col;
      if(st && st.kind){
        if(st.kind === 'lin'){
          const t = c._h ? (iy - st.y0) / (st.y1 - st.y0) : 0;
          col = lerpStops(st.stops, t);
        } else {
          const d = Math.hypot(ix - st.x1, iy - st.y1);
          const t = (d - st.r0) / (st.r1 - st.r0);
          col = lerpStops(st.stops, Math.max(0, Math.min(1, t)));
        }
      } else col = parseColor(st);
      blend(c._px, c._w, ix, iy, col[0], col[1], col[2], col[3]*ga, lighter);
    }
  }
}

function paintImage(c, img, a){
  const lighter = c.globalCompositeOperation === 'lighter';
  const ga = c.globalAlpha;
  let dx, dy, dw, dh, sx = 0, sy = 0;
  if(a.length === 9){ sx=a[0]; sy=a[1]; dw=a[2]; dh=a[3]; dx=a[4]; dy=a[5]; }
  else if(a.length === 4){ dx=a[0]; dy=a[1]; dw=a[2]; dh=a[3]; }
  else { dx=a[0]; dy=a[1]; dw=img._w; dh=img._h; }
  const sw = img._w, sh = img._h, sp = img._px;
  const x0 = Math.max(0, Math.floor(dx)), y0 = Math.max(0, Math.floor(dy));
  const x1 = Math.min(c._w, Math.ceil(dx+dw)), y1 = Math.min(c._h, Math.ceil(dy+dh));
  for(let iy = y0; iy < y1; iy++){
    const sy = Math.min(sh-1, Math.max(0, Math.floor((iy - dy) / dh * sh)));
    for(let ix = x0; ix < x1; ix++){
      const sx = Math.min(sw-1, Math.max(0, Math.floor((ix - dx) / dw * sw)));
      const i = (sy*sw + sx)*3;
      blend(c._px, c._w, ix, iy, sp[i], sp[i+1], sp[i+2], ga, lighter);
    }
  }
}

/* ---- mock DOM ---- */
const mainCanvas = makeCanvas(480, 270);
let rafCb = null;
globalThis.document = {
  getElementById: () => mainCanvas,
  createElement: () => makeCanvas(64, 64)
};
globalThis.requestAnimationFrame = (cb) => { rafCb = cb; };

require('./scene.js');

const t = parseFloat(process.argv[2] || '0');
const out = process.argv[3] || 'frame.ppm';
rafCb(t * 1000);
const buf = Buffer.from(mainCanvas._px);
const head = Buffer.from('P6\n480 270\n255\n');
fs.writeFileSync(out, Buffer.concat([head, buf]));
console.log('rendered t=' + t + 's -> ' + out);
