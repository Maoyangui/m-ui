// radar 工作流的运行摘要:把 radar.mjs 的输出(radar.json)写成 Markdown,工作流里追加到 $GITHUB_STEP_SUMMARY。
//
//   node site/tools/radar-summary.mjs radar.json >> "$GITHUB_STEP_SUMMARY"
//
// 只按 radar.mjs 现在的输出取值:generated、summary{auto,blocked,prohibited,done}、lists[]、mentions{…}。
// awesome-selfhosted 不再是单独字段,从 lists 里按仓库名找。哪个字段缺了、类型不对就写"未知";
// 文件读不到也只在摘要里说明,退出码恒为 0 —— 摘要不能让整个工作流失败
// (以前这一步读早已删掉的 awesomeSelfhosted.eligibleFrom,抛 TypeError,每周的定时运行全部失败)。
import { readFileSync } from 'node:fs';

const UNKNOWN = '未知';
const arr = v => (Array.isArray(v) ? v : null);
const obj = v => (v && typeof v === 'object' && !Array.isArray(v) ? v : {});
const text = v => (typeof v === 'string' && v.trim() ? v.trim() : typeof v === 'number' && Number.isFinite(v) ? String(v) : UNKNOWN);
// 表格单元格与行内文字:去掉换行、转义竖线,一条原因不能把整张表拆坏
const inline = v => text(v).replace(/\s*\r?\n\s*/g, ' ').replace(/\|/g, '\\|');
const link = (label, url) => (typeof url === 'string' && /^https:\/\//.test(url) ? `[${inline(label).replace(/[[\]]/g, '')}](${url})` : inline(label));
const count = v => (arr(v) ? String(v.length) : UNKNOWN);
const names = v => (arr(v) ? v.map(inline).join(', ') || 'none' : UNKNOWN);

function summarize(r) {
  r = obj(r);
  const lines = [`## m-ui radar · ${inline(r.generated)}`, ''];
  const lists = arr(r.lists);

  // awesome-selfhosted:radar.mjs 把它和别的列表一样放进 lists(仓库 awesome-selfhosted/awesome-selfhosted,数据仓库 …-data)
  lines.push('### awesome-selfhosted', '');
  const as = lists && lists.map(obj).find(l => /(^|\/)awesome-selfhosted$/i.test(String(l.repo || '')) || /(^|\/)awesome-selfhosted-data$/i.test(String(l.dataRepo || '')));
  if (as) lines.push(`- ${link(as.repo || as.dataRepo, as.url)} · category **${inline(as.category)}** · eligible from **${inline(as.eligibleFrom)}** · ${inline(as.reason)}`, '');
  else lines.push(`- ${UNKNOWN}(${lists ? '这次扫描的列表里没有 awesome-selfhosted' : 'radar.json 里没有 lists'})`, '');

  const s = obj(r.summary);
  lines.push('### Summary', '', `- auto (PR-able now): ${names(s.auto)}`, `- blocked: ${count(s.blocked)}`, `- prohibited: ${count(s.prohibited)}`, `- done: ${names(s.done)}`, '');
  for (const x of arr(s.blocked) || []) lines.push(`  - blocked: ${inline(x)}`);
  for (const x of arr(s.prohibited) || []) lines.push(`  - prohibited: ${inline(x)}`);

  lines.push('', '### Lists (maintained, ≥20★)', '');
  if (lists) {
    lines.push('| repo | ★ | pushed | category | reason |', '|---|---|---|---|---|');
    for (const l of lists.map(obj)) lines.push(`| ${link(l.repo, l.url)} | ${inline(l.stars)} | ${inline(l.pushed)} | ${inline(l.category)} | ${inline(l.reason)} |`);
  } else lines.push(`- ${UNKNOWN}(radar.json 里没有 lists)`);

  const m = obj(r.mentions);
  lines.push('', '### Mentions', '', `- repositories: ${count(m.repositories)}`, `- issues / discussions: ${count(m.issuesAndDiscussions)}`, `- code: ${count(m.code)}`, '');
  for (const x of (arr(m.repositories) || []).map(obj)) lines.push(`- repo: ${link(x.repo, x.url)} (${inline(x.stars)}★)`);
  for (const x of (arr(m.issuesAndDiscussions) || []).map(obj)) lines.push(`- thread: ${link(x.title, x.url)} · ${inline(x.updated)}`);
  for (const x of (arr(m.code) || []).map(obj)) lines.push(`- code: ${link(`${text(x.repo)} · ${text(x.path)}`, x.url)}`);
  return lines.join('\n') + '\n';
}

// 读文件 → 打印摘要;任何异常都写进摘要,不改退出码
const file = process.argv[2] || 'radar.json';
let out;
try {
  out = summarize(JSON.parse(readFileSync(file, 'utf8')));
} catch (e) {
  out = `## m-ui radar · ${UNKNOWN}\n\n- 摘要没生成出来:${inline(String((e && e.message) || e))}(原始结果见本次运行的 radar artifact)\n`;
}
process.stdout.write(out);
