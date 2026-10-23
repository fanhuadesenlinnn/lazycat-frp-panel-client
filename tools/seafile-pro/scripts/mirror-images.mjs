// Mirror only locked runtime images. This script never publishes a store app.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const root = fileURLToPath(new URL('../', import.meta.url));
const lockPath = path.join(root, 'images.lock.json');
const lock = JSON.parse(fs.readFileSync(lockPath, 'utf8'));
const args = process.argv.slice(2);
if (args.some(arg => arg !== '--apply')) throw new Error('仅支持 --apply；默认只展示计划');
const apply = args.includes('--apply');
const lazyRef = /^registry\.lazycat\.cloud\/[a-zA-Z0-9._/:-]+$/;
const sourceRef = /^[a-z0-9][a-z0-9._/-]*@sha256:[a-f0-9]{64}$/;
const entries = Object.entries(lock.services);
if (entries.length !== 9) throw new Error('基线必须包含九个服务');
for (const [name, service] of entries) {
  if (!sourceRef.test(service.source_digest) || service.architecture !== 'amd64') {
    throw new Error(`源镜像摘要或平台无效：${name}`);
  }
  if (service.lazycat_image && !lazyRef.test(service.lazycat_image)) {
    throw new Error(`已记录的懒猫引用无效：${name}`);
  }
}
if (!apply) {
  for (const [name, service] of entries) {
    console.log(`${name}: ${service.source_digest} -> ${service.lazycat_image || '待转存'}`);
  }
  console.log('仅展示计划；未登录、未发起网络请求、未修改镜像锁文件。');
  process.exit(0);
}

// Require an explicit token so the CLI cannot fall back to interactive login.
if (!process.env.LZC_CLI_TOKEN?.trim()) throw new Error('请通过受限环境变量提供 LZC_CLI_TOKEN，不要放入命令行或代码');
if (!process.env.LZC_CLI_ROOT) throw new Error('请设置 LZC_CLI_ROOT 指向固定版本的官方 CLI 目录');
const cliRoot = path.resolve(process.env.LZC_CLI_ROOT);
const pkg = JSON.parse(fs.readFileSync(path.join(cliRoot, 'package.json'), 'utf8'));
if (pkg.name !== '@lazycatcloud/lzc-cli' || pkg.version !== '2.0.9') {
  throw new Error('必须使用 @lazycatcloud/lzc-cli@2.0.9');
}
for (const [name, service] of entries) {
  if (service.lazycat_image) {
    console.log(`${name}: 已转存，跳过`);
    continue;
  }
  const result = spawnSync(process.execPath, [
    path.join(cliRoot, 'scripts/cli.js'), 'appstore', 'copy-image',
    service.source_digest, '--arch', 'amd64', '--trace-level', 'quiet',
  ], {
    cwd: root, env: process.env, encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'], timeout: 20 * 60 * 1000,
    maxBuffer: 2 * 1024 * 1024,
  });
  // Do not echo raw CLI errors: an authentication failure may contain secrets.
  if (result.error || result.status !== 0) {
    throw new Error(`${name}: 转存未完成；已完成的引用已保存，可重试。未输出原始登录信息。`);
  }
  const refs = [...new Set((result.stdout || '').split(/\r?\n/).map(s => s.trim()).filter(s => lazyRef.test(s)))];
  if (refs.length !== 1) throw new Error(`${name}: 未取得唯一懒猫镜像引用，停止处理`);
  service.lazycat_image = refs[0];
  service.mirrored_at = new Date().toISOString();
  const temporary = `${lockPath}.tmp`;
  fs.writeFileSync(temporary, JSON.stringify(lock, null, 2) + '\n');
  fs.renameSync(temporary, lockPath);
  console.log(`${name}: ${service.lazycat_image}`);
}
console.log('镜像引用已记录；仍需验证镜像内容、构建和测试应用包。未发布商店应用。');
