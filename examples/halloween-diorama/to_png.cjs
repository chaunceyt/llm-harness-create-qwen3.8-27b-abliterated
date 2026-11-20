'use strict';
// PPM -> PNG (no deps): build a minimal 8-bit RGB PNG with a single IDAT.
const fs = require('fs');
const zlib = require('zlib');

function ppmToPng(inPath, outPath){
  const data = fs.readFileSync(inPath);
  let idx = 0; const parts = [];
  while(parts.length < 3){
    let j = data.indexOf(10, idx); parts.push(data.slice(idx, j)); idx = j+1;
    while(data[idx] === 35) j = data.indexOf(10, idx), idx = j+1;
  }
  const w = parseInt(parts[1].toString().split(' ')[0]);
  const h = parseInt(parts[1].toString().split(' ')[1]);
  const raw = data.slice(idx);           // w*h*3

  // raw scanlines with filter byte 0
  const stride = w*3;
  const buf = Buffer.alloc((stride+1)*h);
  for(let y=0;y<h;y++){
    buf[y*(stride+1)] = 0;
    raw.copy(buf, y*(stride+1)+1, y*stride, (y+1)*stride);
  }
  const idat = zlib.deflateSync(buf, { level: 9 });

  function chunk(type, body){
    const len = Buffer.alloc(4); len.writeUInt32BE(body.length);
    const t = Buffer.from(type, 'ascii');
    const crcTable = (()=>{const t=[];for(let n=0;n<256;n++){let c=n;for(let k=0;k<8;k++)c=c&1?0xedb88320^(c>>>1):c>>>1;t[n]=c>>>0;}return t;})();
    const crc = Buffer.alloc(4);
    let c = 0xffffffff;
    for(let i=0;i<t.length;i++){c=crcTable[(c^t[i])&255]^(c>>>8);}
    for(let i=0;i<body.length;i++){c=crcTable[(c^body[i])&255]^(c>>>8);}
    c=(c^0xffffffff)>>>0;
    crc.writeUInt32BE(c);
    return Buffer.concat([len, t, body, crc]);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(w,0); ihdr.writeUInt32BE(h,4);
  ihdr[8]=8; ihdr[9]=2; ihdr[10]=0; ihdr[11]=0; ihdr[12]=0; // 8-bit RGB
  const png = Buffer.concat([
    Buffer.from([0x89,0x50,0x4e,0x47,0x0d,0x0a,0x1a,0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', idat),
    chunk('IEND', Buffer.alloc(0))
  ]);
  fs.writeFileSync(outPath, png);
  console.log(outPath, w+'x'+h, (png.length/1024).toFixed(0)+'KB');
}
ppmToPng(process.argv[2], process.argv[3]);
