// 修正 LPK 内 content.tar 中文件的可执行位（仅在 Windows 上打包后需要）。
//
//   node scripts/fix-lpk-modes.mjs dist/frp-panel-client.lpk [--in-place]
//
// 背景：lzc-cli 用 node-tar 打包 content 目录，tar 头里的 mode 直接取自文件系统。
// Windows 的文件系统无法表达 +x，于是 content/start.sh 与 content/bin/** 的可执行位
// 会在 Windows 上被打成 0644，装到设备上 exec:// 路由会因为脚本不可执行而失败。
// 这个脚本只做一件事：就地改写 tar 头里的 mode 字段（不改动任何文件内容、不改动
// 条目顺序与长度），并重算校验和。正式产物请使用 Linux 构建。
import { readFileSync, writeFileSync, renameSync } from "node:fs";

const BLOCK = 512;
const OUTER_TARGET = "content.tar";

const args = process.argv.slice(2);
const inPlace = args.includes("--in-place");
const pkgPath = args.find((a) => !a.startsWith("--"));
if (!pkgPath) {
  console.error("用法: node scripts/fix-lpk-modes.mjs <文件.lpk> [--in-place]");
  process.exit(2);
}
const outPath = inPlace ? `${pkgPath}.fixed` : (args.includes("--out") ? args[args.indexOf("--out") + 1] : pkgPath.replace(/\.lpk$/, ".fixed.lpk"));

const buf = readFileSync(pkgPath);

function readString(b, off, len) {
  let end = off;
  while (end < off + len && b[end] !== 0) end++;
  return b.toString("utf8", off, end);
}

function readOctal(b, off, len) {
  const s = readString(b, off, len).trim();
  if (s === "") return 0;
  const v = parseInt(s, 8);
  return Number.isNaN(v) ? 0 : v;
}

function writeOctal(b, off, len, value) {
  const s = value.toString(8).padStart(len - 1, "0");
  b.write(s, off, len - 1, "ascii");
  b[off + len - 1] = 0;
}

function isZeroBlock(block) {
  for (let i = 0; i < BLOCK; i++) if (block[i] !== 0) return false;
  return true;
}

// 重算头部校验和（校验和字段本身按空格参与计算）。
function fixChecksum(b, off) {
  b.fill(0x20, off + 148, off + 156);
  let sum = 0;
  for (let i = 0; i < BLOCK; i++) sum += b[off + i];
  const s = sum.toString(8).padStart(6, "0");
  b.write(s, off + 148, 6, "ascii");
  b[off + 154] = 0;
  b[off + 155] = 0x20;
}

function isExecutable(name) {
  return name.endsWith(".sh") || name.startsWith("bin/");
}

/**
 * 遍历一个 tar 缓冲，对每个普通文件条目调用 onEntry(offset, name, mode)。
 * 返回条目的 {offset, name, dataStart, size}。
 */
function walk(b, start, onEntry) {
  const entries = [];
  let off = start;
  let paxPath = null;
  while (off + BLOCK <= b.length) {
    if (isZeroBlock(b.subarray(off, off + BLOCK))) break;
    const rawName = readString(b, off, 100);
    const prefix = readString(b, off + 345, 155);
    let name = prefix ? `${prefix}/${rawName}` : rawName;
    const size = readOctal(b, off + 124, 12);
    const type = String.fromCharCode(b[off + 156]);
    const mode = readOctal(b, off + 100, 8);
    const dataStart = off + BLOCK;
    const dataBlocks = Math.ceil(size / BLOCK);
    const nextOff = dataStart + dataBlocks * BLOCK;

    if (type === "x" || type === "g") {
      // PAX 扩展头：解析 path 覆盖项，作用到下一条目。
      const text = b.toString("utf8", dataStart, dataStart + size);
      for (const line of text.split("\n")) {
        const eq = line.indexOf("=");
        if (eq < 0) continue;
        const key = line.slice(line.indexOf(" ") + 1, eq);
        if (key === "path") paxPath = line.slice(eq + 1);
      }
      off = nextOff;
      continue;
    }
    if (paxPath) {
      name = paxPath;
      paxPath = null;
    }
    if (type === "0" || type === "\0" || type === "" || type === "5") {
      entries.push({ offset: off, name, mode, size, type });
      if (onEntry) onEntry({ offset: off, name, mode, size, type });
    }
    off = nextOff;
  }
  return entries;
}

let patched = 0;
const outer = walk(buf, 0);
const contentEntry = outer.find((e) => e.name.replace(/^\.\//, "") === OUTER_TARGET);
if (!contentEntry) {
  console.error(`未在 LPK 中找到 ${OUTER_TARGET}，不是本工具预期的 LPK 结构`);
  process.exit(1);
}

walk(buf, contentEntry.offset + BLOCK, (e) => {
  if (e.type === "5") return; // 目录
  if (!isExecutable(e.name.replace(/^\.\//, ""))) return;
  if (e.mode === 0o755) return;
  writeOctal(buf, e.offset + 100, 8, 0o755);
  fixChecksum(buf, e.offset);
  patched++;
  console.log(`  mode 0644 -> 0755  ${e.name}`);
});

// 外层条目的模式不影响解包结果，但也顺手规范化，避免出现 0664 之类的怪值。
for (const e of outer) {
  if (e.type === "0" && e.mode !== 0o644) {
    writeOctal(buf, e.offset + 100, 8, 0o644);
    fixChecksum(buf, e.offset);
  }
}

writeFileSync(outPath, buf);
if (inPlace) renameSync(outPath, pkgPath);
console.log(`已修正 ${patched} 个条目的可执行位 -> ${inPlace ? pkgPath : outPath}`);
