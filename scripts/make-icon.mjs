// 生成懒猫应用图标 icon.png（256x256，RGBA），不依赖任何第三方库。
//
//   node scripts/make-icon.mjs
//
// 设计：深墨蓝圆角方块 + 薄荷绿“隧道/穿透”箭头 + 琥珀色入口节点，
// 4 倍超采样后降采样，保证小尺寸下边缘依然干净。
import { deflateSync } from "node:zlib";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const SIZE = 256;
const SS = 4; // supersampling factor
const N = SIZE * SS;

const INK_TOP = [12, 24, 38];
const INK_BOTTOM = [16, 56, 66];
const MINT = [47, 224, 192];
const AMBER = [255, 194, 75];
const PIPE = [232, 246, 245];

function mix(a, b, t) {
  return [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t];
}

function roundRectSDF(px, py, cx, cy, halfW, halfH, r) {
  const qx = Math.abs(px - cx) - (halfW - r);
  const qy = Math.abs(py - cy) - (halfH - r);
  const ax = Math.max(qx, 0);
  const ay = Math.max(qy, 0);
  return Math.hypot(ax, ay) + Math.min(Math.max(qx, qy), 0) - r;
}

// 朝右的箭头：矩形箭杆 + 等腰三角箭头，返回到形状边界的近似有符号距离。
// 传 dir = -1 时把 x 关于轴心镜像，就得到朝左的箭头。
function arrowSDF(px, py, x0, x1, y, thickness, head, dir = 1) {
  const axis = (x0 + x1) / 2;
  if (dir < 0) px = 2 * axis - px;

  const shaftHalf = thickness / 2;
  const shaftEnd = x1 - head;
  const shaft = roundRectSDF(
    px, py, (x0 + shaftEnd) / 2, y,
    Math.max(0.01, (shaftEnd - x0) / 2), shaftHalf, shaftHalf * 0.5,
  );

  const vx = px - shaftEnd;
  const vy = py - y;
  let tri = Infinity;
  if (vx >= -head && vx <= head) {
    const headHalf = head * 1.45;
    const maxY = headHalf * (1 - vx / head);
    tri = Math.abs(vy) - maxY;
  }
  return Math.min(shaft, tri);
}

function coverage(sdf, aa) {
  // 1 inside, 0 outside, smooth across ~1.5 device pixels
  return Math.min(1, Math.max(0, 0.5 - sdf / aa));
}

const ss = SS; // device pixels per logical pixel
const aaHalf = ss * 0.75;

// 累积缓冲区（逻辑像素）
const acc = new Float64Array(SIZE * SIZE * 4);

const cx = N / 2;
const cy = N / 2;
const cardHalf = N * 0.5 - 0.5 * ss; // 铺满整幅，圆角
const radius = N * 0.235;

// 隧道：一条水平管 + 两个箭头（进 / 出）
const pipeHalfH = N * 0.085;
const pipeLeft = N * 0.20;
const pipeRight = N * 0.80;
const arrowThickness = N * 0.062;
const arrowHead = N * 0.085;

for (let y = 0; y < N; y++) {
  for (let x = 0; x < N; x++) {
    const px = x + 0.5;
    const py = y + 0.5;

    // 背景：圆角方块 + 垂直渐变
    const card = coverage(roundRectSDF(px, py, cx, cy, cardHalf, cardHalf, radius), aaHalf);
    let color = mix(INK_TOP, INK_BOTTOM, py / N);
    let alpha = card;

    // 管道底纹（浅色，低不透明度）
    const pipe = coverage(roundRectSDF(px, py, cx, cy, (pipeRight - pipeLeft) / 2, pipeHalfH, pipeHalfH), aaHalf);
    if (pipe > 0) {
      color = mix(color, PIPE, pipe * 0.16 * card);
    }

    // 上箭头（向右，薄荷绿）
    const up = coverage(arrowSDF(px, py, pipeLeft, pipeRight, cy - N * 0.055, arrowThickness, arrowHead, 1), aaHalf);
    if (up > 0) color = mix(color, MINT, up * card);

    // 下箭头（向左）
    const down = coverage(arrowSDF(px, py, pipeLeft, pipeRight, cy + N * 0.055, arrowThickness, arrowHead, -1), aaHalf);
    if (down > 0) color = mix(color, MINT, down * card);

    // 入口节点（左侧琥珀色圆点）
    const dotR = N * 0.045;
    const dotCx = N * 0.115;
    const dot = coverage(Math.hypot(px - dotCx, py - cy) - dotR, aaHalf);
    if (dot > 0) {
      const glow = coverage(Math.hypot(px - dotCx, py - cy) - dotR * 2.4, aaHalf);
      color = mix(color, AMBER, Math.max(dot, glow * 0.18) * card);
    }

    const lx = Math.floor(x / SS);
    const ly = Math.floor(y / SS);
    const base = (ly * SIZE + lx) * 4;
    acc[base] += color[0] * alpha;
    acc[base + 1] += color[1] * alpha;
    acc[base + 2] += color[2] * alpha;
    acc[base + 3] += alpha;
  }
}

const samples = SS * SS;
const rgba = Buffer.alloc(SIZE * SIZE * 4);
for (let i = 0; i < SIZE * SIZE; i++) {
  const base = i * 4;
  const a = acc[base + 3] / samples;
  rgba[base] = clamp255(acc[base] / samples);
  rgba[base + 1] = clamp255(acc[base + 1] / samples);
  rgba[base + 2] = clamp255(acc[base + 2] / samples);
  rgba[base + 3] = clamp255(a * 255);
}

function clamp255(v) {
  return Math.max(0, Math.min(255, Math.round(v)));
}

// ---- 最小 PNG 编码器（8 位 RGBA，无隔行） ----
const raw = Buffer.alloc(SIZE * (SIZE * 4 + 1));
for (let y = 0; y < SIZE; y++) {
  raw[y * (SIZE * 4 + 1)] = 0; // filter: none
  rgba.copy(raw, y * (SIZE * 4 + 1) + 1, y * SIZE * 4, (y + 1) * SIZE * 4);
}

function crcTable() {
  const table = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c;
  }
  return table;
}
const CRC = crcTable();

function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = CRC[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([len, body, crc]);
}

const ihdr = Buffer.alloc(13);
ihdr.writeUInt32BE(SIZE, 0);
ihdr.writeUInt32BE(SIZE, 4);
ihdr[8] = 8; // bit depth
ihdr[9] = 6; // color type: RGBA
ihdr[10] = 0;
ihdr[11] = 0;
ihdr[12] = 0;

const png = Buffer.concat([
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  chunk("IHDR", ihdr),
  chunk("IDAT", deflateSync(raw, { level: 9 })),
  chunk("IEND", Buffer.alloc(0)),
]);

const out = join(dirname(fileURLToPath(import.meta.url)), "..", "icon.png");
writeFileSync(out, png);
console.log(`已生成 ${out}（${png.length} 字节，${SIZE}x${SIZE}）`);
