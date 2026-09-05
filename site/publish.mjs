// 把到了日期的文章发到 GitHub Discussions(General 分类),让技术内容在仓库里也可被搜到。
//
//   node site/build.mjs && GH_TOKEN=… node site/publish.mjs
//
// 依据 site/dist/content.json:discussion=true 且 discussionDate ≤ 今天 且站点已发布的文章。
// 已发过的靠正文里的原文链接判断(直接翻最近的 discussions,不依赖搜索索引),
// 所以不需要在仓库里记状态、不产生提交。跑几次都幂等。
import { readFileSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = dirname(fileURLToPath(import.meta.url));
const REPO = 'Maoyangui/m-ui';
const [OWNER, NAME] = REPO.split('/');
const CATEGORY = process.env.DISCUSSION_CATEGORY || 'General';
const TODAY = new Date().toISOString().slice(0, 10);
const manifestPath = join(ROOT, 'dist', 'content.json');
if (!existsSync(manifestPath)) throw new Error('先运行 node site/build.mjs 生成 site/dist/content.json');
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
const SITE = manifest.site, GITHUB = manifest.github;

const gql = (query, vars) => {
  const args = ['api', 'graphql', '-f', `query=${query}`];
  for (const [k, v] of Object.entries(vars)) args.push('-f', `${k}=${v}`);
  return JSON.parse(execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 1 << 24 }));
};

// 文章正文 → Discussions 正文:GitHub 的 Markdown 直接渲染这些 HTML 标签,只需要把站内链接换成绝对地址
function bodyOf(item) {
  let html = readFileSync(join(ROOT, item.source), 'utf8');
  html = html.replace(/^<!--meta[\s\S]*?-->/, '');
  html = html.replace(/<nav class="toc">[\s\S]*?<\/nav>/g, '').replace(/<p class="meta">[\s\S]*?<\/p>/g, '').replace(/<script[\s\S]*?<\/script>/g, '');
  html = html.replace(/<article class="doc">|<\/article>/g, '');
  html = html.replaceAll('{{base}}', SITE).replaceAll('{{github}}', GITHUB).replaceAll('{{author_url}}', 'https://github.com/' + OWNER);
  html = html.replace(/<h1>[\s\S]*?<\/h1>/, ''); // 标题已是 discussion 标题
  html = html.replace(/ class="[^"]*"/g, '').replace(/ loading="lazy"/g, '');
  const intro = item.lang === 'zh'
    ? `> 本文来自 m-ui(Maoyangui 的 sing-box 面板)的文档站,原文:${item.url}\n\n`
    : `> From the docs of m-ui, the sing-box panel by Maoyangui. Canonical: ${item.url}\n\n`;
  const outro = item.lang === 'zh'
    ? `\n\n---\n原文与更新:${item.url} · 源码:${GITHUB} · 提问请在 Q&A 分类。`
    : `\n\n---\nCanonical page: ${item.url} · Source: ${GITHUB} · Questions belong in the Q&A category.`;
  return intro + html.trim() + outro;
}

const repo = gql(`query($o:String!,$n:String!){repository(owner:$o,name:$n){id discussionCategories(first:20){nodes{id name}}}}`, { o: OWNER, n: NAME }).data.repository;
const cat = repo.discussionCategories.nodes.find(c => c.name === CATEGORY);
if (!cat) { console.log(`没有 ${CATEGORY} 分类,跳过`); process.exit(0); }

const recent = gql(`query($o:String!,$n:String!){repository(owner:$o,name:$n){discussions(first:100,orderBy:{field:CREATED_AT,direction:DESC}){nodes{body}}}}`, { o: OWNER, n: NAME }).data.repository.discussions.nodes;
let created = 0;
for (const item of manifest.articles) {
  if (!item.discussion || !item.published || !item.discussionDate || item.discussionDate > TODAY) continue;
  // 原文链接每篇每种语言唯一(中文在 /zh/ 下),正文开头就有它;直接翻最近的 discussions 比搜索可靠
  if (recent.some(d => (d.body || '').includes(item.url))) { console.log('已发过:', item.lang + '/' + item.slug); continue; }
  const r = gql(`mutation($r:ID!,$c:ID!,$t:String!,$b:String!){createDiscussion(input:{repositoryId:$r,categoryId:$c,title:$t,body:$b}){discussion{url}}}`,
    { r: repo.id, c: cat.id, t: item.title, b: bodyOf(item) });
  console.log('已发布:', r.data.createDiscussion.discussion.url);
  created++;
}
const due = manifest.articles.filter(a => a.date === TODAY).length;
console.log(`本次新发 ${created} 篇;今天到站点发布日的文章 ${due} 篇`);
if (process.env.GITHUB_OUTPUT) {
  const { appendFileSync } = await import('node:fs');
  appendFileSync(process.env.GITHUB_OUTPUT, `created=${created}\ndue=${due}\n`);
}
