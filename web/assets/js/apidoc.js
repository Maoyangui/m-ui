// 外部 API 的接口文档(接口一览 + curl 示例):管理员页与代理面板「我的账号」共用这一份,点「接口文档」在右侧抽屉里打开。
// web/apidoc_test.go 核对:这里的路径与 docs/API.md 的「接口一览」一致、每个路径在外部 API 下都有处理、perm 与后端的 apiPermFor 一致。
import { t } from './i18n.js';
import { esc, badge, openDrawer, registerActions, copy, setHTML } from './ui.js';

// m 方法、p 路径(接在 …/api/v1 后面)、k 说明的 i18n 键、g 归在哪一组、perm 需要的权限('' = 密钥有效就行);
// idem = 支持幂等键,dry = 支持 dryRun 预览,admin = 只有主面板的密钥能调
export const API_ENDPOINTS = [
  { m: 'GET', p: '/ping', k: 'adm.ep.ping', g: 'panel', perm: '' },
  { m: 'GET', p: '/plans', k: 'adm.ep.plans', g: 'plan', perm: 'plans.read' },
  { m: 'GET', p: '/users', k: 'adm.ep.users', g: 'user', perm: 'users.read' },
  { m: 'POST', p: '/users', k: 'adm.ep.create', g: 'user', perm: 'users.create' },
  { m: 'GET', p: '/users/{name|id}', k: 'adm.ep.get', g: 'user', perm: 'users.read' },
  { m: 'PATCH / PUT', p: '/users/{name|id}', k: 'adm.ep.update', g: 'user', perm: 'users.update' },
  { m: 'DELETE', p: '/users/{name|id}', k: 'adm.ep.delete', g: 'user', perm: 'users.delete' },
  { m: 'POST', p: '/users/{name|id}/enable', k: 'adm.ep.enable', g: 'act', perm: 'users.update' },
  { m: 'POST', p: '/users/{name|id}/disable', k: 'adm.ep.disable', g: 'act', perm: 'users.update' },
  { m: 'POST', p: '/users/{name|id}/reset', k: 'adm.ep.reset', g: 'act', perm: 'users.update' },
  { m: 'POST', p: '/users/{name|id}/kick', k: 'adm.ep.kick', g: 'act', perm: 'users.update' },
  { m: 'POST', p: '/users/{name|id}/rotate', k: 'adm.ep.rotate', g: 'act', perm: 'users.update' },
  { m: 'POST', p: '/users/{name|id}/plan', k: 'adm.ep.plan', g: 'plan', perm: 'billing', idem: true, dry: true },
  { m: 'POST', p: '/users/{name|id}/renew', k: 'adm.ep.renew', g: 'plan', perm: 'billing', idem: true, dry: true },
  { m: 'POST', p: '/users/{name|id}/change', k: 'adm.ep.change', g: 'plan', perm: 'billing', idem: true, dry: true },
  { m: 'GET', p: '/users/{name|id}/plan-ops', k: 'adm.ep.planOps', g: 'plan', perm: 'users.read' },
  { m: 'GET', p: '/users/{name|id}/sub', k: 'adm.ep.sub', g: 'data', perm: 'users.read' },
  { m: 'GET', p: '/users/{name|id}/traffic', k: 'adm.ep.traffic', g: 'data', perm: 'users.read' },
  { m: 'GET', p: '/overview', k: 'adm.ep.overview', g: 'panel', perm: 'panel.read' },
  { m: 'GET', p: '/servers', k: 'adm.ep.servers', g: 'panel', perm: 'panel.read', admin: true },
  { m: 'GET', p: '/lines', k: 'adm.ep.lines', g: 'panel', perm: 'panel.read' },
  { m: 'GET', p: '/onlines', k: 'adm.ep.onlines', g: 'panel', perm: 'panel.read' },
];

// 文档里各组的先后:先是最常对接的用户与套餐,面板信息放最后
const GROUPS = ['user', 'act', 'plan', 'data', 'panel'];

// apiExample curl 示例。reseller 为真时是代理版:用户与套餐都是代理自己的。
// 接口地址和密钥放在最前面两个变量里,下面每条命令都短、能整段复制去跑(以前每一行都带着整串地址和密钥,换行换得没法看)。
export function apiExample(base, token, reseller = false) {
  base = String(base || '').replace(/\/+$/, '');
  const auth = '-H "Authorization: Bearer $KEY"';
  const json = '-H "Content-Type: application/json"';
  const user = reseller ? 'dl-001' : 'alice';
  const plan = t(reseller ? 'adm.ex.rsPlan' : 'adm.ex.plan');
  const better = t(reseller ? 'adm.ex.rsPlanUp' : 'adm.ex.planUp');
  return [
    `API="${base}"`,
    `KEY="${token}"`,
    '',
    `# ${t('adm.ex.create')}`,
    `curl -X POST "$API/users" ${auth} ${json} \\`,
    `  -d '{"name":"${user}","plan":"${plan}","remark":"${t('adm.ex.order')} #1001"}'`,
    '',
    `# ${t('adm.ex.renew')}`,
    `curl -X POST "$API/users/${user}/renew" ${auth} ${json} \\`,
    `  -H "Idempotency-Key: order-1002" -d '{"periods":3,"mode":"extend"}'`,
    '',
    `# ${t('adm.ex.preview')}`,
    `curl -X POST "$API/users/${user}/change" ${auth} ${json} \\`,
    `  -d '{"plan":"${better}","mode":"stack","dryRun":true}'`,
    `curl -X POST "$API/users/${user}/change" ${auth} ${json} \\`,
    `  -H "Idempotency-Key: order-1003" -d '{"plan":"${better}","mode":"stack"}'`,
    '',
    `# ${t('adm.ex.misc')}`,
    `curl -X POST "$API/users/${user}/disable" ${auth}`,
    `curl -X POST "$API/users/${user}/rotate" ${auth}`,
    `curl "$API/users/${user}/plan-ops" ${auth}`,
    `curl "$API/users/${user}/sub" ${auth}`,
  ].join('\n');
}

// ---- 抽屉 ----
// ctx:{ base 接口地址, reseller 是不是代理面板, keys 这个面板的密钥(示例里可以选一把填进去), repo 仓库地址 }
let ctx = { base: '', reseller: false, keys: [], repo: '' };
let pick = 0; // 示例里填哪一把密钥(id;0 = 写占位符)

const keyName = k => k.name || t('api.keyDefault');
const exampleText = () => {
  const k = ctx.keys.find(x => x.id === pick);
  return apiExample(ctx.base, k ? k.token : t('adm.exKeyPh'), ctx.reseller);
};

function endpoint(e) {
  const tags = [e.idem ? badge(t('adm.apiIdem'), 'primary') : '', e.dry ? badge(t('adm.apiDry')) : ''].filter(Boolean).join(' ');
  // 方法按动作上色:读是绿的、建和动作是蓝的、改是黄的、删是红的,扫一眼就知道哪些会改东西
  const kind = e.m.startsWith('GET') ? 'get' : e.m.startsWith('DELETE') ? 'del' : e.m.startsWith('PATCH') ? 'put' : 'post';
  return `<div class="ep"><span class="ep-m ${kind}">${esc(e.m)}</span><code class="ep-p">${esc(e.p)}</code>
    <span class="ep-perm" title="${esc(t('adm.apiNeed'))}">${esc(e.perm ? t('api.perm.' + e.perm) : t('adm.apiAnyKey'))}</span>
    <span class="ep-d">${esc(t(e.k))}${tags ? ' ' + tags : ''}</span></div>`;
}

function docsHTML() {
  const eps = API_ENDPOINTS.filter(e => !(ctx.reseller && e.admin));
  const docs = (ctx.repo || 'https://github.com/Maoyangui/m-ui') + '/blob/main/docs/API.md';
  const sel = ctx.keys.length
    ? `<select id="apidoc-key" class="sm" data-change="apidoc.key" title="${esc(t('adm.apiExKey'))}">${ctx.keys.map(k => `<option value="${k.id}" ${k.id === pick ? 'selected' : ''}>${esc(keyName(k))}</option>`).join('')}</select>`
    : '';
  return `<section class="doc-how">
      <p class="hint">${esc(t(ctx.reseller ? 'adm.apiHowRs' : 'adm.apiHow'))}</p>
      <dl class="kv">
        <dt>${t('adm.apiBase')}</dt><dd><div class="sub-box"><code>${esc(ctx.base)}</code><button class="btn sm" data-act="apidoc.copy" data-id="${esc(ctx.base)}">${t('common.copy')}</button></div></dd>
        <dt>${t('adm.apiHeader')}</dt><dd><div class="sub-box"><code>Authorization: Bearer &lt;${esc(t('api.keyToken'))}&gt;</code></div></dd>
      </dl>
    </section>
    ${GROUPS.map(g => {
    const list = eps.filter(e => e.g === g);
    return list.length ? `<section><h3>${t('adm.g.' + g)}</h3><div class="ep-list">${list.map(endpoint).join('')}</div></section>` : '';
  }).join('')}
    <section>
      <p class="hint doc-note">${esc(t('adm.apiIdemHelp'))}</p>
      <div class="doc-ex-head"><h3>${t('adm.apiExample')}</h3>${sel}<button class="btn sm" data-act="apidoc.copyEx">${t('common.copy')}</button></div>
      <pre class="log code doc-ex" id="apidoc-ex">${esc(exampleText())}</pre>
      <p class="hint">${t(ctx.reseller ? 'acct.apiDocs' : 'adm.apiDocs')} <a href="${esc(docs)}" target="_blank" rel="noopener">docs/API.md</a></p>
    </section>`;
}

// openApiDocs 打开接口文档。keys 里启用着的第一把默认填进示例(没有密钥、或外部 API 还没开时示例里写占位符)。
export function openApiDocs(opts) {
  ctx = { base: '', reseller: false, keys: [], repo: '', ...opts };
  const first = ctx.keys.find(k => k.enabled) || ctx.keys[0];
  pick = first ? first.id : 0;
  openDrawer(esc(t('adm.apiDocsTitle')), docsHTML(), { size: 'xl' });
}

registerActions({
  'apidoc.copy': id => copy(id),
  'apidoc.copyEx': () => copy(exampleText()),
  'apidoc.key': (_, el) => { pick = Number(el.value) || 0; setHTML('apidoc-ex', esc(exampleText())); },
});
