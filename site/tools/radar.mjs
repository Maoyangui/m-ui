// 每周雷达:用 GitHub API 找两样东西,结果写成 JSON 供工作流存成 artifact(不进仓库历史)。
//  1. 外链机会:与 sing-box / 代理面板相关、仍在维护的 awesome / curated 列表,以及它们是否已收录 m-ui、
//     README 里有没有"欢迎 PR / 禁止自荐"的信号;awesome-selfhosted 按"首个 Release 满 4 个月"规则算出可提交日期。
//  2. 第三方提及:GitHub 上提到 Maoyangui/m-ui 或站点地址的仓库 / Issue / Discussion(排除本仓库)。
//
//   GH_TOKEN=… node site/tools/radar.mjs > radar.json
//
// 只读、不提交、不留言;PR 需要能 fork 到别人仓库的令牌,Actions 的 GITHUB_TOKEN 做不到,所以这里只给出候选与判断。
import { execFileSync } from 'node:child_process';

const REPO = 'Maoyangui/m-ui';
const SITE = 'https://maoyangui.github.io/m-ui/';
const FIRST_RELEASE = '2026-09-02'; // v0.1.0
const TODAY = new Date().toISOString().slice(0, 10);
const api = (path, params = {}) => {
  const args = ['api', '-X', 'GET', path];
  for (const [k, v] of Object.entries(params)) args.push('-f', `${k}=${v}`);
  try { return JSON.parse(execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 1 << 26 })); } catch (e) { return null; }
};
const daysAgo = iso => Math.floor((Date.now() - new Date(iso).getTime()) / 86400000);
const addMonths = (iso, n) => { const d = new Date(iso); d.setUTCMonth(d.getUTCMonth() + n); return d.toISOString().slice(0, 10); };

// ---- 1. 外链机会 ----
const listQueries = [
  'awesome sing-box in:name,description,readme', 'awesome singbox in:name,description,readme',
  'awesome proxy panel in:readme', 'awesome self-hosted vpn in:readme', 'sing-box tools list in:readme',
  'topic:awesome-list sing-box', 'topic:awesome-list proxy', 'topic:awesome sing-box',
];
const seen = new Map();
for (const q of listQueries) {
  const r = api('search/repositories', { q, sort: 'stars', per_page: '15' });
  for (const it of (r && r.items) || []) if (!seen.has(it.full_name) && it.full_name !== REPO) seen.set(it.full_name, it);
}
const lists = [];
for (const it of seen.values()) {
  if (daysAgo(it.pushed_at) > 180 || it.stargazers_count < 20 || it.archived) continue;
  // 只要"列表型"仓库,而且主题真和 sing-box / 代理面板沾边;巨型通用 awesome 列表不算机会
  const meta = ((it.name || '') + ' ' + (it.description || '')).toLowerCase();
  if (!/awesome|curated|list|directory|collection|资源|合集/.test(meta)) continue;
  if (!/proxy|vpn|sing-?box|xray|v2ray|self-?hosted|selfhosted|sysadmin|network/.test(meta)) continue; // 主题本身就要沾边,不看 README 顺带一提
  const readme = api(`repos/${it.full_name}/readme`, {});
  const text = readme && readme.content ? Buffer.from(readme.content, 'base64').toString('utf8') : '';
  const low = text.toLowerCase();
  if (!/sing-box|singbox/.test(low) && !/(proxy|vpn) (panel|tool|server)|xray|v2ray|hysteria|shadowsocks/.test(low)) continue;
  lists.push({
    repo: it.full_name, url: it.html_url, stars: it.stargazers_count, pushed: it.pushed_at.slice(0, 10), description: it.description || '',
    mentionsMui: /maoyangui\/m-ui|maoyangui\.github\.io\/m-ui/.test(low),
    acceptsPR: /pull request|contribut/.test(low),
    forbidsSelfPromo: /no self[- ]promot|self[- ]promotion (is )?not allowed|don'?t submit your own/.test(low),
    aboutSingBox: /sing-box|singbox/.test(low),
    aboutProxyPanel: /proxy|vpn|panel|xray|v2ray/.test(low),
  });
}
lists.sort((a, b) => b.stars - a.stars);
const awesomeSelfhosted = { repo: 'awesome-selfhosted/awesome-selfhosted-data', rule: 'first release older than 4 months', eligibleFrom: addMonths(FIRST_RELEASE, 4), eligible: TODAY >= addMonths(FIRST_RELEASE, 4) };

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

const out = { generated: TODAY, site: SITE, repo: REPO, backlinkCandidates: lists, awesomeSelfhosted, mentions };
console.log(JSON.stringify(out, null, 2));
