// Uses the pinned official CLI; credentials are supplied only through Secrets.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import { spawnSync } from 'node:child_process';

// 官方 CLI 的位置：CI 里 npm 装到 .tools/，本地开发通常放在仓库外的 .build-tools/。
function resolveCliRoot() {
  const candidates = [
    process.env.LZC_CLI_ROOT,
    '.tools/node_modules/@lazycatcloud/lzc-cli',
    '../.build-tools/node_modules/@lazycatcloud/lzc-cli',
    '.build-tools/node_modules/@lazycatcloud/lzc-cli',
  ].filter(Boolean);
  for (const candidate of candidates) {
    const resolved = path.resolve(candidate);
    if (fs.existsSync(path.join(resolved, 'package.json'))) return resolved;
  }
  throw new Error(
    '未找到 lzc-cli：请先 npm install --prefix .tools @lazycatcloud/lzc-cli@2.0.9，或用 LZC_CLI_ROOT 指定已有安装目录',
  );
}

const cliRoot = resolveCliRoot();
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
  const username = (process.env.LAZYCAT_USERNAME || '').trim();
  let password = process.env.LAZYCAT_PASSWORD || '';
  if (!username || !password) throw new Error('Missing LAZYCAT_USERNAME / LAZYCAT_PASSWORD Secrets');
  // 从网页或文本里复制密码时，首尾很容易带上空格/换行，账号服务会直接判为“密码错误”。
  // 密码不会以空白开头或结尾，这里忽略首尾空白并提示（不打印密码本身）。
  if (password !== password.trim()) {
    console.error('提示：LAZYCAT_PASSWORD 首尾含空白字符，已忽略首尾空白后重试。');
    password = password.trim();
  }
  const { default: config } = await load('config/env.js');
  // Keep the returned token in memory; never persist it to the CLI config file.
  config.save = () => {};
  const { reLogin } = await load('appstore/login.js');
  try {
    await reLogin({ username, password });
  } catch (error) {
    throw new Error(
      `${error?.message || 'login failed'}（请确认 LAZYCAT_USERNAME / LAZYCAT_PASSWORD 就是登录 developer.lazycat.cloud 的社区账号，密码首尾不要带空格或换行）`,
    );
  }
  const token = String(config.get('token') || '').trim();
  if (!token || /[\r\n]/.test(token)) throw new Error('Login did not return a valid token');
  if (!process.env.GITHUB_ENV) {
    // 本地自检模式：只验证账号可用，不写出任何文件，也不打印令牌。
    console.log('登录成功（本地自检模式，未写入任何文件）。');
    return;
  }
  console.log(`::add-mask::${token}`);
  fs.appendFileSync(process.env.GITHUB_ENV, `LZC_CLI_TOKEN=${token}\n`, { mode: 0o600 });
}

// 打包完成后直接读 LPK 里的 manifest.yml，确认上架包里引用的就是懒猫官方源镜像。
async function assertLpkReferences(lpkPath, ref) {
  const tar = requireCLI('tar');
  const tmpDir = fs.mkdtempSync(path.join('dist', '.lpk-check-'));
  try {
    await tar.x({ file: lpkPath, cwd: tmpDir, filter: (entry) => entry === 'manifest.yml' });
    const manifestPath = path.join(tmpDir, 'manifest.yml');
    if (!fs.existsSync(manifestPath)) throw new Error('LPK 内缺少 manifest.yml');
    const image = yaml.load(fs.readFileSync(manifestPath, 'utf8'))?.application?.image;
    if (image !== ref) throw new Error(`LPK 内 application.image 不是官方源引用: ${image}`);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
}

// 官方 CLI 的“顺带创建应用”路径会把失败原因吞掉（只打印一段去开发者中心手动创建的说明），
// 这里自己走一遍：查存在性 → 预检 → 创建，并把服务端返回的状态和内容打出来。
// 应用已经存在时直接返回，不改动任何商店信息。
async function ensureStoreApp(pkg) {
  const { request } = await load('appstore/login.js');
  const base = 'https://appstore.api.lazycat.cloud/api/v3/developer';

  const check = await request(`${base}/app/check/exist?package=${encodeURIComponent(pkg.package)}`, { method: 'GET' });
  const checkBody = (await check.text()).trim();
  console.log(`商店应用存在性检查: status=${check.status} body=${checkBody.slice(0, 200)}`);
  if (check.ok) {
    let exists = false;
    try {
      exists = JSON.parse(checkBody)?.exist === true;
    } catch {
      exists = false;
    }
    if (exists) {
      console.log(`开发者中心已存在应用 ${pkg.package}，跳过创建。`);
      return;
    }
  }

  const preflight = await request(`${base}/app/create`, { method: 'OPTIONS' });
  console.log(`app/create 预检: status=${preflight.status}`);

  const payload = {
    package: pkg.package,
    language: 'zh',
    name: pkg.name,
    source: pkg.homepage || '',
    source_author: pkg.author || '',
  };
  const create = await request(`${base}/app/create`, { method: 'POST', body: JSON.stringify(payload) });
  const createBody = (await create.text()).trim();
  console.log(`app/create 创建: status=${create.status} body=${createBody.slice(0, 400)}`);
  if (!create.ok) {
    // 懒猫明确禁止用接口创建应用，必须人工在开发者平台建好，所以这里给出可执行的说明。
    throw new Error(
      `开发者中心还没有应用 ${pkg.package}，而懒猫不允许通过接口创建（status=${create.status}: ${createBody.slice(0, 200)}）。\n` +
        '请先用浏览器打开 https://developer.lazycat.cloud/manage，登录开发者账号后点“新增”，' +
        `把应用包标识符填成 ${pkg.package}（必须完全一致），名称填 ${pkg.name}，创建成功后再重跑本工作流即可提交审核。`,
    );
  }
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
  await assertLpkReferences(lpk, ref);
  runCLI(['lpk', 'lint', lpk]);
  runCLI(['lpk', 'info', lpk]);

  const { Publish } = await load('appstore/publish.js');
  const warnings = await Publish.collectPreCheckWarnings(lpk);
  if (warnings.length) throw new Error(`Store pre-check failed: ${warnings.map((w) => w.code ?? w.message).join(', ')}`);
  // 商店要求应用先在开发者中心存在；CLI 自带的创建流程失败时只给一段说明，这里自己创建并暴露真实原因。
  await ensureStoreApp(pkg);
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
  } catch (error) {
    const message = String(error?.message || '');
    // 应用资料（图标/简介/截图/支持平台）只能在开发者平台网页里填，接口改不了，
    // 这里把服务端的原因连同一份可照做的清单一起抛出来。
    if (message.includes('开发者平台提交应用信息')) {
      throw new Error(
        `${message}\n` +
          `提示：请到 https://developer.lazycat.cloud/manage 打开应用 ${pkg.package} 的应用信息，` +
          '至少勾选“支持桌面端”，并补齐图标、简介、截图等资料，保存后重跑本工作流即可。',
      );
    }
    throw error;
  } finally {
    clearTimeout(watchdog);
  }
  if (!response?.ok) throw new Error('Review submission was not confirmed');
  const receipt = { package: pkg.package, version: pkg.version, image: ref, arch, submitted: true, submittedAt: new Date().toISOString() };
  fs.writeFileSync('dist/submission.json', `${JSON.stringify(receipt, null, 2)}\n`);
  fs.writeFileSync('dist/lzc-manifest.yml', fs.readFileSync('lzc-manifest.yml'));
  console.log('LPK submitted for review; approval is pending.');
}

// 只描述凭据的“结构”（长度、字符类别、首尾是否有空白/引号），绝不打印凭据本身。
// 用来排查“从网页/手机复制粘贴进 GitHub Secrets 时带进了隐藏字符”这类问题。
function charClasses(text) {
  const classes = { 小写: 0, 大写: 0, 数字: 0, 符号: 0, 空白: 0, 非ASCII: 0 };
  for (const char of text) {
    if (char >= 'a' && char <= 'z') classes['小写'] += 1;
    else if (char >= 'A' && char <= 'Z') classes['大写'] += 1;
    else if (char >= '0' && char <= '9') classes['数字'] += 1;
    else if (/\s/.test(char)) classes['空白'] += 1;
    else if (char.codePointAt(0) > 0x7f) classes['非ASCII'] += 1;
    else classes['符号'] += 1;
  }
  return Object.entries(classes)
    .filter(([, count]) => count > 0)
    .map(([name, count]) => `${name}×${count}`)
    .join(' ');
}

function describeSecret(name, raw) {
  if (!raw) {
    console.log(`${name}: 未设置或为空`);
    return;
  }
  const trimmed = raw.trim();
  const edge = [raw[0], raw[raw.length - 1]]
    .filter((char) => char !== undefined && /\s/.test(char))
    .length;
  console.log(
    `${name}: 长度=${raw.length} 首尾空白=${raw !== trimmed} 内部连续空白=${/\s{2,}/.test(trimmed)} ` +
      `首尾含引号=${/^["'`]|["'`]$/.test(raw)} 类别=${charClasses(trimmed)}`,
  );
  if (edge) console.log(`  （长度差 ${raw.length - trimmed.length} 个字符是首尾空白）`);
}

async function check() {
  const rawUsername = process.env.LAZYCAT_USERNAME || '';
  const rawPassword = process.env.LAZYCAT_PASSWORD || '';
  describeSecret('LAZYCAT_USERNAME', rawUsername);
  if (rawUsername.trim().length > 4) {
    const name = rawUsername.trim();
    console.log(`LAZYCAT_USERNAME 预览: ${name.slice(0, 2)}***${name.slice(-2)}`);
  }
  describeSecret('LAZYCAT_PASSWORD', rawPassword);
  console.log('（以上只输出长度与字符类别，不输出凭据内容）');
  await login();
}

try {
  if (process.argv[2] === 'login') await login();
  else if (process.argv[2] === 'publish') await publish();
  else if (process.argv[2] === 'check') await check();
  else throw new Error('Expected login, publish or check');
} catch (error) {
  // Never dump response bodies or exception objects containing credentials.
  let message = String(error?.message || 'Lazycat operation failed');
  for (const secret of [process.env.LAZYCAT_USERNAME, process.env.LAZYCAT_PASSWORD, process.env.LZC_CLI_TOKEN]) {
    if (secret) message = message.split(secret).join('***');
  }
  console.error(message);
  process.exitCode = 1;
}
