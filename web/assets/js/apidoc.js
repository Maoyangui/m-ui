// 外部 API 的接口一览与 curl 示例:管理员页与代理面板「我的账号」共用这一份。
// web/apidoc_test.go 核对这里的路径与 docs/API.md 的「接口一览」一致,而且每个路径在外部 API 下都有处理。
import { t } from './i18n.js';
import { esc, badge } from './ui.js';

// m 方法、p 路径(接在 …/api/v1 后面)、k 说明的 i18n 键;idem = 支持幂等键,dry = 支持 dryRun 预览
export const API_ENDPOINTS = [
  { m: 'GET', p: '/ping', k: 'adm.ep.ping' },
  { m: 'GET', p: '/plans', k: 'adm.ep.plans' },
  { m: 'GET', p: '/users', k: 'adm.ep.users' },
  { m: 'POST', p: '/users', k: 'adm.ep.create' },
  { m: 'GET', p: '/users/{name|id}', k: 'adm.ep.get' },
  { m: 'PATCH / PUT', p: '/users/{name|id}', k: 'adm.ep.update' },
  { m: 'DELETE', p: '/users/{name|id}', k: 'adm.ep.delete' },
  { m: 'POST', p: '/users/{name|id}/enable', k: 'adm.ep.enable' },
  { m: 'POST', p: '/users/{name|id}/disable', k: 'adm.ep.disable' },
  { m: 'POST', p: '/users/{name|id}/reset', k: 'adm.ep.reset' },
  { m: 'POST', p: '/users/{name|id}/kick', k: 'adm.ep.kick' },
  { m: 'POST', p: '/users/{name|id}/rotate', k: 'adm.ep.rotate' },
  { m: 'POST', p: '/users/{name|id}/plan', k: 'adm.ep.plan', idem: true, dry: true },
  { m: 'POST', p: '/users/{name|id}/renew', k: 'adm.ep.renew', idem: true, dry: true },
  { m: 'POST', p: '/users/{name|id}/change', k: 'adm.ep.change', idem: true, dry: true },
  { m: 'GET', p: '/users/{name|id}/plan-ops', k: 'adm.ep.planOps' },
  { m: 'GET', p: '/users/{name|id}/sub', k: 'adm.ep.sub' },
];

// apiTable 接口一览表:方法、路径、说明(支持幂等 / 预览的打标)
export function apiTable() {
  const tags = e => [e.idem ? badge(t('adm.apiIdem'), 'primary') : '', e.dry ? badge(t('adm.apiDry')) : ''].filter(Boolean).join(' ');
  return `<div class="table-wrap"><table class="grid api-doc"><thead><tr><th>${t('adm.apiMethod')}</th><th>${t('adm.apiPath')}</th><th>${t('adm.apiDesc')}</th></tr></thead><tbody>
    ${API_ENDPOINTS.map(e => `<tr><td class="mono">${esc(e.m)}</td><td class="mono">${esc(e.p)}</td><td>${esc(t(e.k))}${tags(e) ? ' ' + tags(e) : ''}</td></tr>`).join('')}
  </tbody></table></div>
  <p class="hint">${esc(t('adm.apiIdemHelp'))}</p>`;
}

// apiExample curl 示例。reseller 为真时是代理版:用户与套餐都是代理自己的
export function apiExample(base, token, reseller = false) {
  base = String(base || '').replace(/\/+$/, '');
  const auth = `-H "Authorization: Bearer ${token}" -H "Content-Type: application/json"`;
  const user = reseller ? 'dl-001' : 'alice';
  const plan = t(reseller ? 'adm.ex.rsPlan' : 'adm.ex.plan');
  const better = t(reseller ? 'adm.ex.rsPlanUp' : 'adm.ex.planUp');
  return [
    `# ${t('adm.ex.create')}`,
    `curl -X POST ${auth} \\`,
    `  -d '{"name":"${user}","plan":"${plan}","remark":"${t('adm.ex.order')} #1001"}' ${base}/users`,
    '',
    `# ${t('adm.ex.renew')}`,
    `curl -X POST ${auth} -H "Idempotency-Key: order-1002" \\`,
    `  -d '{"periods":3,"mode":"extend"}' ${base}/users/${user}/renew`,
    '',
    `# ${t('adm.ex.preview')}`,
    `curl -X POST ${auth} \\`,
    `  -d '{"plan":"${better}","mode":"stack","dryRun":true}' ${base}/users/${user}/change`,
    `curl -X POST ${auth} -H "Idempotency-Key: order-1003" \\`,
    `  -d '{"plan":"${better}","mode":"stack"}' ${base}/users/${user}/change`,
    '',
    `# ${t('adm.ex.misc')}`,
    `curl -X POST -H "Authorization: Bearer ${token}" ${base}/users/${user}/disable`,
    `curl -X POST -H "Authorization: Bearer ${token}" ${base}/users/${user}/rotate`,
    `curl -H "Authorization: Bearer ${token}" ${base}/users/${user}/plan-ops`,
    `curl -H "Authorization: Bearer ${token}" ${base}/users/${user}/sub`,
  ].join('\n');
}
