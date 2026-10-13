// Uses the pinned official CLI; credentials are supplied only through Secrets.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import { spawnSync } from 'node:child_process';

const cliRoot = path.resolve(process.env.LZC_CLI_ROOT || '.tools/node_modules/@lazycatcloud/lzc-cli');
const requireCLI = createRequire(path.join(cliRoot, 'package.json'));
const yaml = requireCLI('js-yaml');
const logger = requireCLI('loglevel');
logger.setLevel('info');
const cli = path.join(cliRoot, 'scripts/cli.js');
const load = (file) => import(pathToFileURL(path.join(cliRoot, 'lib', file)).href);

// inquirer 是双入口包（exports.import -> dist/esm/index.mjs, exports.require -> dist/cjs/index.js）。
// 官方 publish.js 走 ESM，而 require.resolve('inquirer') 命中的是 CJS 入口，两者不是
// 同一个对象，直接覆写那个 CJS 包装只会得到 __esModule/default 双层结构、改不到真实实例。
// 因此在 CLI 包目录里临时落一个 ESM 桥接模块，让它按 "import" 条件解析到与官方
// 模块完全相同的那个 inquirer 实例，再覆写它的 prompt。
async function patchCliPrompt(defaults) {
  const bridge = path.join(cliRoot, `.lzc-store-bridge-${process.pid}.mjs`);
  const source = [
    "import inquirer from 'inquirer';",
    'export function patchPrompt(values) {',
    '  inquirer.prompt = async (questions) => {',
    '    const list = Array.isArray(questions) ? questions : [questions];',
    '    const answers = {};',
    '    for (const question of list) {',
    '      const key = question && question.name;',
    '      if (!key || !Object.prototype.hasOwnProperty.call(values, key)) {',
    '        throw new Error(`Unexpected CLI prompt: ${key}`);',
    '      }',
    '      answers[key] = values[key];',
    '    }',
    '    return answers;',
    '  };',
    '}',
    '',
  ].join('\n');
  fs.writeFileSync(bridge, source);
  try {
    const mod = await import(pathToFileURL(bridge).href);
    mod.patchPrompt(defaults);
  } finally {
    fs.rmSync(bridge, { force: true });
  }
}

// 只替换 application.image 那一行，保留 manifest 里的注释与排版。
function setManifestImage(ref) {
  const lines = fs.readFileSync('lzc-manifest.yml', 'utf8').split(/\r?\n/);
  let patched = 0;
  const out = lines.map((line) => {
    if (patched === 0 && /^\s*image:/.test(line)) {
      patched = 1;
      return line.replace(/^(\s*)image:.*$/, `$1image: ${ref}`);
    }
    return line;
  });
  if (patched !== 1) throw new Error('lzc-manifest.yml 中未找到唯一的 image: 行');
  const next = out.join('\n');
  if (yaml.load(next)?.application?.image !== ref) throw new Error('manifest 镜像引用替换失败');
  fs.writeFileSync('lzc-manifest.yml', next);
}

function runCLI(args, capture = false) {
  const result = spawnSync(process.execPath, [cli, ...args], {
    env: process.env,
    encoding: 'utf8',
    timeout: 20 * 60 * 1000,
    maxBuffer: 2 * 1024 * 1024,
    stdio: capture ? ['ignore', 'pipe', 'pipe'] : ['ignore', 'inherit', 'inherit'],
  });
  if (result.error || result.status !== 0) throw new Error(`Lazycat CLI failed: ${args[0]} ${args[1]}`);
  return result.stdout || '';
}

async function login() {
  const username = process.env.LAZYCAT_USERNAME;
  const password = process.env.LAZYCAT_PASSWORD;
  if (!username || !password) throw new Error('Missing LAZYCAT_USERNAME / LAZYCAT_PASSWORD Secrets');
  if (!process.env.GITHUB_ENV) throw new Error('Login must run in GitHub Actions');
  const { default: config } = await load('config/env.js');
  // Keep the returned token in memory; never persist it to the CLI config file.
  config.save = () => {};
  const { reLogin } = await load('appstore/login.js');
  await reLogin({ username, password });
  const token = config.get('token');
  if (!token || /[\r\n]/.test(token)) throw new Error('Login did not return a valid token');
  console.log(`::add-mask::${token}`);
  fs.appendFileSync(process.env.GITHUB_ENV, `LZC_CLI_TOKEN=${token}\n`, { mode: 0o600 });
}

async function publish() {
  if (!process.env.LZC_CLI_TOKEN) throw new Error('Missing login token');
  const image = process.env.PUBLIC_IMAGE;
  const arch = process.env.STORE_ARCH || 'amd64';
  if (!/^ghcr\.io\/[a-z0-9_.\/-]+:[a-z0-9_.-]+$/.test(image || '')) throw new Error('Invalid public image');
  if (!['amd64', 'arm64'].includes(arch)) throw new Error('Invalid target architecture');
  fs.mkdirSync('dist', { recursive: true });
  const copied = runCLI(['appstore', 'copy-image', image, '--arch', arch, '--trace-level', 'quiet'], true);
  const refs = copied.split(/\r?\n/).map((line) => line.trim()).filter((line) => /^registry\.lazycat\.cloud\/[A-Za-z0-9_.\/-]+:[A-Za-z0-9_.-]+$/.test(line));
  if (refs.length !== 1) throw new Error('Copy-image did not return exactly one official image reference');
  const ref = refs[0];
  const pkg = yaml.load(fs.readFileSync('package.yml', 'utf8'));
  setManifestImage(ref);
  fs.writeFileSync('dist/IMAGE.txt', `image: ${ref}\nsource: ${image}\narch: ${arch}\n`);
  console.log(`Official image (${arch}): ${ref}`);
  const lpk = `dist/lazycat-frp-panel-client-${pkg.version}.lpk`;
  runCLI(['project', 'release', '-f', 'lzc-build.yml', '-o', lpk]);
  runCLI(['lpk', 'lint', lpk]);
  runCLI(['lpk', 'info', lpk]);

  const { Publish } = await load('appstore/publish.js');
  const warnings = await Publish.collectPreCheckWarnings(lpk);
  if (warnings.length) throw new Error(`Store pre-check failed: ${warnings.map((w) => w.code ?? w.message).join(', ')}`);
  // The CLI prompts only when creating the first store listing. Supply the
  // package metadata; unexpected prompts fail instead of hanging a CI job.
  await patchCliPrompt({
    continue: 'y',
    name: pkg.name,
    package: pkg.package,
    source: pkg.homepage || '',
    author: pkg.author || '',
  });
  const changelog = (process.env.RELEASE_CHANGELOG || '客户端界面与镜像体积优化。').trim();
  if (!changelog) throw new Error('Empty changelog');
  // 兜底：交互式提示一旦没被拦下就会挂死 CI，超过这个时间直接失败。
  const watchdog = setTimeout(() => {
    console.error('发布超时（可能出现了未预期的交互式提示）');
    process.exit(1);
  }, 20 * 60 * 1000);
  let response;
  try {
    response = await new Publish().publish(lpk, { zh: changelog }, 'zh');
  } finally {
    clearTimeout(watchdog);
  }
  if (!response?.ok) throw new Error('Review submission was not confirmed');
  const receipt = { package: pkg.package, version: pkg.version, image: ref, arch, submitted: true, submittedAt: new Date().toISOString() };
  fs.writeFileSync('dist/submission.json', `${JSON.stringify(receipt, null, 2)}\n`);
  fs.writeFileSync('dist/lzc-manifest.yml', fs.readFileSync('lzc-manifest.yml'));
  console.log('LPK submitted for review; approval is pending.');
}

try {
  if (process.argv[2] === 'login') await login();
  else if (process.argv[2] === 'publish') await publish();
  else throw new Error('Expected login or publish');
} catch (error) {
  // Never dump response bodies or exception objects containing credentials.
  let message = String(error?.message || 'Lazycat operation failed');
  for (const secret of [process.env.LAZYCAT_USERNAME, process.env.LAZYCAT_PASSWORD, process.env.LZC_CLI_TOKEN]) {
    if (secret) message = message.split(secret).join('***');
  }
  console.error(message);
  process.exitCode = 1;
}
