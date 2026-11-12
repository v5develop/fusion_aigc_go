// Renders deterministic synthetic images x params with the REAL web implementation
// (applyColorFixPixels from colorfix-math.ts) into raw RGBA golden files.
//
//   node parity/gen_golden.ts <outDir>
//   COLORFIX_MATH_TS=/path/to/colorfix-math.ts  (default: the sibling fusion-aigc-web repo)
//
// Output: <outDir>/manifest.json + <name>.in.rgba / <name>.<i>.out.rgba (w*h*4 bytes).
import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const DEFAULT_MATH = '/Users/apple/Project/Code/fusion-aigc-web/src/utils/colorfix-math.ts'
const mathPath = process.env.COLORFIX_MATH_TS || DEFAULT_MATH
const outDir = process.argv[2]
if (!outDir) {
  console.error('usage: node gen_golden.ts <outDir>')
  process.exit(2)
}
const { applyColorFixPixels } = (await import(pathToFileURL(mathPath).href)) as {
  applyColorFixPixels: (data: Uint8ClampedArray, p: Record<string, number>, mask?: Uint8Array | null) => void
}
mkdirSync(outDir, { recursive: true })

function mulberry32(seed: number) {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const W = 64
const H = 48
function makeImage(kind: string, rnd: () => number): Uint8ClampedArray {
  const d = new Uint8ClampedArray(W * H * 4)
  for (let y = 0; y < H; y++) {
    for (let x = 0; x < W; x++) {
      const i = (y * W + x) * 4
      let r = 0, g = 0, b = 0
      if (kind === 'noise') {
        r = rnd() * 256; g = rnd() * 256; b = rnd() * 256
      } else if (kind === 'ramps') {
        r = (x / (W - 1)) * 255; g = (y / (H - 1)) * 255; b = ((x + y) / (W + H - 2)) * 255
      } else if (kind === 'levels') { // every level 0..255 on every channel in a different phase
        const v = (y * W + x) % 256
        r = v; g = (v + 85) % 256; b = (v + 170) % 256
      } else if (kind === 'cube') { // 4x4x4 colour cube incl. pure black / white / primaries
        const n = (x >> 2) + 16 * (y >> 2)
        r = ((n & 3) * 255) / 3; g = (((n >> 2) & 3) * 255) / 3; b = (((n >> 4) & 3) * 255) / 3
      } else if (kind === 'fabric') { // garment-like: base colour + smooth shading + fine noise
        const shade = 0.75 + 0.25 * Math.sin(x / 7) * Math.cos(y / 9)
        r = (150 * shade) + rnd() * 10; g = (60 * shade) + rnd() * 10; b = (70 * shade) + rnd() * 10
      } else if (kind === 'dark') {
        r = rnd() * 40; g = rnd() * 40; b = rnd() * 40
      } else if (kind === 'bright') {
        r = 215 + rnd() * 40; g = 215 + rnd() * 40; b = 215 + rnd() * 40
      } else if (kind === 'saturated') {
        const k = (x + y) % 3
        r = k === 0 ? 255 : rnd() * 30; g = k === 1 ? 255 : rnd() * 30; b = k === 2 ? 255 : rnd() * 30
      }
      d[i] = r; d[i + 1] = g; d[i + 2] = b; d[i + 3] = 255 - ((x * 3 + y) % 7) * 20 // alpha must be preserved
    }
  }
  return d
}

const RANGES: Record<string, [number, number]> = {
  hue: [-30, 30], sat: [-60, 70], light: [-40, 40], bright: [-80, 80], contrast: [-50, 50],
}
const KEYS = Object.keys(RANGES)
const rnd = mulberry32(20261112)
const paramSets: Record<string, number>[] = [
  { hue: 0, sat: 0, light: 0, bright: 0, contrast: 0 },
  { hue: 3, sat: -16, light: -6, bright: -17, contrast: 10 }, // COLORFIX_DEFAULT
]
for (const k of KEYS) {
  for (const v of RANGES[k]) paramSets.push({ hue: 0, sat: 0, light: 0, bright: 0, contrast: 0, [k]: v })
}
paramSets.push(Object.fromEntries(KEYS.map((k) => [k, RANGES[k][0]])))
paramSets.push(Object.fromEntries(KEYS.map((k) => [k, RANGES[k][1]])))
for (let i = 0; i < 24; i++) paramSets.push(Object.fromEntries(KEYS.map((k) => [k, Math.round(RANGES[k][0] + rnd() * (RANGES[k][1] - RANGES[k][0]))])))
for (let i = 0; i < 6; i++) paramSets.push(Object.fromEntries(KEYS.map((k) => [k, RANGES[k][0] + rnd() * (RANGES[k][1] - RANGES[k][0])]))) // fractional

const kinds = ['noise', 'ramps', 'levels', 'cube', 'fabric', 'dark', 'bright', 'saturated']
const manifest: { name: string; w: number; h: number; input: string; cases: { params: Record<string, number>; expected: string }[] }[] = []
for (const kind of kinds) {
  const img = makeImage(kind, rnd)
  writeFileSync(join(outDir, `${kind}.in.rgba`), img)
  const cases = []
  paramSets.forEach((p, i) => {
    const copy = new Uint8ClampedArray(img)
    applyColorFixPixels(copy, p)
    const f = `${kind}.${i}.out.rgba`
    writeFileSync(join(outDir, f), copy)
    cases.push({ params: p, expected: f })
  })
  manifest.push({ name: kind, w: W, h: H, input: `${kind}.in.rgba`, cases })
}
writeFileSync(join(outDir, 'manifest.json'), JSON.stringify(manifest))
console.log(`golden: ${kinds.length} images x ${paramSets.length} param sets -> ${outDir}`)
