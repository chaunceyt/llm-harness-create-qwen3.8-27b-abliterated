# Midnight Rain — Lo-Fi Halloween Loop

An animated pixel-art Halloween diorama, built to be the looping cover for a
10-hour lofi music video. One self-contained file: **`index.html`**
(no dependencies, no network, no build step).

## Run it
```
# just open it, or:
python3 -m http.server 8000        # then visit http://localhost:8000
```
Fullscreen (F11) gives a crisp 1080p cover. It auto-scales to any display.

## How it works
- **Crisp 1080p** — drawn at 480×270 logical pixels and upscaled 4× to
  1920×1080 with `image-rendering: pixelated`, so every pixel is a clean
  4×4 block at full screen. `object-fit: cover` keeps it full-bleed on any
  window size.
- **Strong warm/cold contrast** — a cold indigo/teal rainy night (moon, sky,
  wet stones, blue rain) against warm amber pockets: glowing windows, a
  porch lantern, a street lamp, and a flickering jack-o-lantern.
- **Living effects** — 3 depth layers of slanting rain with ground splashes,
  rising chimney smoke, drifting ground fog, fire sparks off the pumpkin,
  flickering light (windows/lantern/lamp/lantern core), shimmering wet-ground
  reflections, rain ripples, a crossing bat, a twitching cat tail, a drifting
  cloud passing the moon, and twinkling stars.

## Seamless for hours
Every animation is a pure function of `t = now mod 12s` using only *integer*
cycles per loop (sines with whole frequencies, `mod 1` phases, stepped
flicker harmonics). That makes the 12-second loop **mathematically
seamless** — frame(t) === frame(t + 12s) exactly — so it can loop for 10
hours (or forever) with no visible hitch.

The static backdrop is rendered once to an offscreen canvas; only the
dynamic layers are redrawn per frame (~735 draw ops/frame — trivial for any
GPU), keeping it smooth at 60fps indefinitely.

## Verification included
- `scene.js` — the scene script extracted from `index.html` (used by tests).
- `harness.cjs` — runs a mock-DOM simulation of 720 frames and proves the
  loop is seamless (draw-call sequences at t, t+12s, and t+24s are byte-identical).
- `raster.cjs` / `to_png.cjs` — a dependency-free software rasterizer that
  renders real frames to PNG (`frame_t3.png`, `frame_t6.png` are samples).

Run the proof:
```
node harness.cjs        # -> SEAMLESS LOOP VERIFIED
node raster.cjs 3 f3.ppm && node to_png.cjs f3.ppm f3.png
```
