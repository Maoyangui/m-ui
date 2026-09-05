// 推广雷达 + 执行器:找机会,并把每个机会分成四类,能自动做的直接做,不能做的说清楚为什么。
//
//   GH_TOKEN=… node site/tools/radar.mjs [--act] > radar.json
//
// 四类:
//   auto      GitHub 上接受 PR、不禁止自荐、也不禁止机器提交、主题相关、m-ui 已达到其收录门槛的 curated 列表 → --act 时直接 fork + PR
//   browser   V2EX / Show HN / Reddit Megathread / LowEndTalk / Telegram:要浏览器登录态;有登录态且版规允许时由当前会话发布,发完记进 site/launches.json
//   blocked   需要令牌 / 手机 / 人工验证,或列表要求的门槛(如首个 Release 满 N 个月)还没到 → 给出可重试日期
//   prohibited 站规禁止自动发帖或 AI 内容(LINUX DO),或列表明确禁止机器/LLM 生成的提交 → 永久跳过,不绕
//
// 状态在 site/launches.json:同一个 launch 不会每周再发一次;发布成功由脚本或人追加一条 {platform,url,date,type}。
// Actions 里的 GITHUB_TOKEN 不能 fork 别人的仓库,所以 --act 只在本机(gh 已登录)有意义;工作流里只出报告。
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const REPO = 'Maoyangui/m-ui';
const SITE = 'https://maoyangui.github.io/m-ui/';
const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const STATE = join(ROOT, 'launches.json');
const ACT = process.argv.includes('--act');
const TODAY = new Date().toISOString().slice(0, 10);
const state = existsSync(STATE) ? JSON.parse(readFileSync(STATE, 'utf8')) : { firstRelease: '2026-09-02', launches: [], platforms: {} };
const FIRST_RELEASE = state.firstRelease || '2026-09-02';

const api = (path, params = {}) => {
  const args = ['api', '-X', 'GET', path];
  for (const [k, v] of Object.entries(params)) args.push('-f', `${k}=${v}`);
  try { return JSON.parse(execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 1 << 26 })); } catch { return null; }
};
const b64 = c => c && c.content ? Buffer.from(c.content.replace(/\n/g, ''), 'base64').toString('utf8') : '';
const daysAgo = iso => Math.floor((Date.now() - new Date(iso).getTime()) / 86400000);
const addMonths = (iso, n) => { const d = new Date(iso); d.setUTCMonth(d.getUTCMonth() + n); return d.toISOString().slice(0, 10); };
const done = (platform, type) => state.launches.find(l => l.platform === platform && (!type || l.type === type));

// ---- 1. GitHub 列表 ----
const listQueries = [
  'awesome sing-box in:name,description,readme', 'awesome singbox in:name,description,readme',
  'awesome proxy panel in:readme', 'awesome self-hosted vpn in:readme', 'sing-box tools list in:readme',
  'topic:awesome-list sing-box', 'topic:awesome-list proxy', 'topic:awesome sing-box', 'topic:awesome-list sysadmin vpn',
];
const seen = new Map();
for (const q of listQueries) {
  const r = api('search/repositories', { q, sort: 'stars', per_page: '15' });
  for (const it of (r && r.items) || []) if (!seen.has(it.full_name) && it.full_name !== REPO) seen.set(it.full_name, it);
}
const lists = [];
for (const it of seen.values()) {
  if (daysAgo(it.pushed_at) > 180 || it.stargazers_count < 20 || it.archived) continue;
  const meta = ((it.name || '') + ' ' + (it.description || '')).toLowerCase();
  if (!/awesome|curated|list|directory|collection|资源|合集/.test(meta)) continue;
  if (!/proxy|vpn|sing-?box|xray|v2ray|self-?hosted|selfhosted|sysadmin|network/.test(meta)) continue;
  const readme = b64(api(`repos/${it.full_name}/readme`, {})).toLowerCase();
  if (!/sing-box|singbox/.test(readme) && !/(proxy|vpn) (panel|tool|server)|xray|v2ray|hysteria|shadowsocks|wireguard/.test(readme)) continue;
  // 贡献规则:README 里的 + 仓库(或它的 -data 数据仓库)的 CONTRIBUTING
  const dataRepo = /awesome-(selfhosted|sysadmin)$/.test(it.full_name) ? it.full_name + '-data' : it.full_name;
  const contrib = (b64(api(`repos/${dataRepo}/contents/CONTRIBUTING.md`, {})) || b64(api(`repos/${dataRepo}/contents/.github/CONTRIBUTING.md`, {}))).toLowerCase();
  const rules = readme + '\n' + contrib;
  const ageMonths = (rules.match(/(?:older than|at least|minimum of|released for)\s+(\d+)\s+months?/) || [])[1];
  const humanOnly = /(machine|llm|ai)[- ]generated (contributions|submissions|pull requests)[^.]*not allowed|no (ai|llm)[- ]generated/.test(rules);
  const eligibleFrom = ageMonths ? addMonths(FIRST_RELEASE, Number(ageMonths)) : null;
  const cand = {
    repo: it.full_name, dataRepo, url: it.html_url, stars: it.stargazers_count, pushed: it.pushed_at.slice(0, 10), description: it.description || '',
    mentionsMui: /maoyangui\/m-ui|maoyangui\.github\.io\/m-ui/.test(rules),
    acceptsPR: /pull request|contribut/.test(rules),
    forbidsSelfPromo: /no self[- ]promot|self[- ]promotion (is )?not allowed|don'?t submit your own/.test(rules),
    humanOnly, eligibleFrom, aboutSingBox: /sing-box|singbox/.test(readme),
    yamlData: contrib.includes('software/') && contrib.includes('.yml'),
  };
  cand.category = cand.mentionsMui ? 'done'
    : humanOnly ? 'prohibited'
    : !cand.acceptsPR || cand.forbidsSelfPromo ? 'blocked'
    : eligibleFrom && TODAY < eligibleFrom ? 'blocked'
    : 'auto';
  cand.reason = cand.mentionsMui ? '已收录'
    : humanOnly ? '该列表明确禁止机器 / LLM 生成的提交,只能由人手工提交'
    : !cand.acceptsPR ? '不接受 PR' : cand.forbidsSelfPromo ? '禁止自荐'
    : eligibleFrom && TODAY < eligibleFrom ? `要求首个 Release 满 ${ageMonths} 个月,${eligibleFrom} 起可提交`
    : '可以提交 PR';
  lists.push(cand);
}
lists.sort((a, b) => b.stars - a.stars);

// ---- 2. 第三方提及 ----
const mentionQueries = ['"Maoyangui/m-ui"', '"maoyangui.github.io/m-ui"'];
const mentions = { repositories: [], issuesAndDiscussions: [], code: [] };
for (const q of mentionQueries) {
  const r = api('search/repositories', { q: `${q} in:readme,description -repo:${REPO}`, per_page: '20' });
  for (const it of (r && r.items) || []) if (!mentions.repositories.some(x => x.repo === it.full_name)) mentions.repositories.push({ repo: it.full_name, url: it.html_url, stars: it.stargazers_count, query: q });
  const i = api('search/issues', { q: `${q} -repo:${REPO}`, per_page: '20' });
  for (const it of (i && i.items) || []) if (!mentions.issuesAndDiscussions.some(x => x.url === it.html_url)) mentions.issuesAndDiscussions.push({ title: it.title, url: it.html_url, updated: it.updated_at.slice(0, 10), query: q });
}
for (const q of ['maoyangui.github.io/m-ui', 'Maoyangui/m-ui']) {
  const c = api('search/code', { q: `"${q}" -repo:${REPO}`, per_page: '20' });
  for (const it of (c && c.items) || []) if (!mentions.code.some(x => x.url === it.html_url)) mentions.code.push({ repo: it.repository.full_name, path: it.path, url: it.html_url });
}

// ---- 3. 浏览器渠道:按 launches.json 里的状态与各站规则给出"该做什么" ----
const redditFrom = addMonths(FIRST_RELEASE, 3);
const channels = [
  { platform: 'v2ex', category: 'browser', url: 'https://v2ex.com/go/create', done: !!done('v2ex'), note: '分享创造节点欢迎发布自己的作品;需要已登录的浏览器会话' },
  { platform: 'hackernews', category: 'browser', url: 'https://news.ycombinator.com/submit', done: !!done('hackernews'), note: 'Show HN;Live Demo 满足"能直接试"的要求;需要已登录的浏览器会话' },
  { platform: 'reddit-selfhosted', category: 'browser', url: 'https://www.reddit.com/r/selfhosted/', done: !!done('reddit-selfhosted'),
    note: TODAY < redditFrom ? `不满 3 个月:只能发在当期 New Project Megathread;${redditFrom} 起重读版规决定能否单独发帖` : '已满 3 个月:先重读 r/selfhosted 当前版规,允许再单独发帖', recheckOn: redditFrom },
  { platform: 'lowendtalk', category: 'browser', url: 'https://lowendtalk.com/', done: !!done('lowendtalk'), note: '已登录 + 实时核对版规 + 找到允许介绍开源工具的分类 + 明确自称作者,四条都满足才发' },
  { platform: 'telegram', category: 'browser', url: 'https://web.telegram.org/', done: !!done('telegram'), note: '只在群规明确允许开源项目分享的公开群发一次;不私聊、不群发' },
  { platform: 'linuxdo', category: 'prohibited', url: 'https://linux.do/', done: false, note: '站规禁止自动发帖与 AI 生成/润色内容,永久跳过' },
];

// ---- 4. --act:对 auto 类的 YAML 数据列表直接 fork + PR(本机 gh 登录态;Actions 的令牌不能 fork)----
const actions = [];
function openListPR(c) {
  if (!c.yamlData) return { repo: c.dataRepo, status: 'skipped', reason: '不是 YAML 数据仓库,条目格式各异,不自动改 README' };
  const me = execFileSync('gh', ['api', 'user', '--jq', '.login'], { encoding: 'utf8' }).trim();
  const [owner, name] = c.dataRepo.split('/');
  if (!api(`repos/${me}/${name}`, {})) execFileSync('gh', ['repo', 'fork', c.dataRepo, '--clone=false'], { stdio: 'ignore' });
  const branch = 'add-m-ui';
  const base = api(`repos/${c.dataRepo}`, {}).default_branch;
  const sha = api(`repos/${c.dataRepo}/git/ref/heads/${base}`, {}).object.sha;
  try { execFileSync('gh', ['api', '-X', 'POST', `repos/${me}/${name}/git/refs`, '-f', `ref=refs/heads/${branch}`, '-f', `sha=${sha}`], { stdio: 'ignore' }); } catch { /* 分支已存在 */ }
  const yml = [
    'name: m-ui', `website_url: ${SITE}`,
    'description: Embedded sing-box control plane for multi-server proxy/VPN deployments, one Go binary with dry-run validation and rollback (alternative to 3x-ui, s-ui)',
    'licenses:', '  - GPL-3.0', 'platforms:', '  - Go', 'tags:', '  - VPN', `source_code_url: https://github.com/${REPO}`, '',
  ].join('\n');
  execFileSync('gh', ['api', '-X', 'PUT', `repos/${me}/${name}/contents/software/m-ui.yml`, '-f', 'message=add m-ui', '-f', `branch=${branch}`, '-f', `content=${Buffer.from(yml).toString('base64')}`], { stdio: 'ignore' });
  const body = `Adds m-ui (https://github.com/${REPO}), an embedded sing-box control plane for multi-server deployments. I maintain this project. Live demo: ${SITE}demo/ · docs: ${SITE}`;
  const out = execFileSync('gh', ['pr', 'create', '--repo', c.dataRepo, '--head', `${me}:${branch}`, '--base', base, '--title', 'Add m-ui', '--body', body], { encoding: 'utf8' }).trim();
  state.launches.push({ platform: 'github-list', url: out, date: TODAY, type: c.dataRepo });
  return { repo: c.dataRepo, status: 'pr-opened', url: out };
}
if (ACT) {
  for (const c of lists.filter(x => x.category === 'auto' && !done('github-list', x.dataRepo))) {
    try { actions.push(openListPR(c)); } catch (e) { actions.push({ repo: c.dataRepo, status: 'failed', reason: String(e.message || e).slice(0, 200) }); }
  }
  if (actions.some(a => a.status === 'pr-opened')) writeFileSync(STATE, JSON.stringify(state, null, 2) + '\n');
}

const out = {
  generated: TODAY, site: SITE, repo: REPO, firstRelease: FIRST_RELEASE, act: ACT,
  summary: {
    auto: lists.filter(l => l.category === 'auto').map(l => l.repo),
    blocked: lists.filter(l => l.category === 'blocked').map(l => `${l.repo}: ${l.reason}`).concat(channels.filter(c => c.category === 'browser' && !c.done).map(c => `${c.platform}: 需要浏览器登录态`)),
    prohibited: lists.filter(l => l.category === 'prohibited').map(l => `${l.repo}: ${l.reason}`).concat(channels.filter(c => c.category === 'prohibited').map(c => `${c.platform}: ${c.note}`)),
    done: lists.filter(l => l.category === 'done').map(l => l.repo).concat(channels.filter(c => c.done).map(c => c.platform)),
  },
  lists, channels, launches: state.launches, actions, mentions,
};
console.log(JSON.stringify(out, null, 2));
