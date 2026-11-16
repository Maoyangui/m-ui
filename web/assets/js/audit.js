// 审计记录怎么读:对象(key)和动作(action)在库里是英文短词("user" / "batch:enable"),这里译成界面上的话。
// 概况的「最近操作」和日志页的「操作审计」共用,两处的说法才一样。认不出的原样显示(新版后端加了动作、前端还没跟上时不至于空着)。
import { t } from './i18n.js';

// 定时任务写审计的执行者名(和后端 model.JobActors 同一份):按名单排除,不按「以 Job 结尾」,名字以 job 结尾的人不会被藏掉
export const JOB_ACTORS = ['ResetJob', 'DepleteJob', 'RuleJob', 'FollowJob', 'GuardJob'];

const has = k => { const v = t(k); return v && v !== k ? v : ''; };
// 对象:多数就是导航里那一页的名字
const OBJ_NAV = { user: 'nav.users', line: 'nav.lines', upstream: 'nav.upstreams', node: 'nav.nodes', plan: 'nav.plans', rule: 'nav.rules', reseller: 'nav.resellers',
  settings: 'nav.settings', backup: 'nav.backup', cert: 'nav.cert', ext: 'nav.exts', ops: 'nav.ops', admin: 'nav.admin', logs: 'nav.logs', fw: 'nav.fw' };
export function auditObject(key) {
  return OBJ_NAV[key] ? t(OBJ_NAV[key]) : has('audit.o.' + key) || String(key || '');
}

const verb = v => has('audit.v.' + v) || v;
// auditAction 动作一句话。套餐相关的几种带着套餐名;「前缀:后面」的(批量、执行某个运维任务、清某类日志、导入某个文件)拆开译前缀
export function auditAction(r) {
  const key = r.key, a = String(r.action || '');
  let m;
  if (key === 'user') {
    if ((m = /^batch:plan:(renew|extend|bind)(?::(.*))?$/.exec(a))) return m[1] === 'bind' ? t('dash.audit.batchBind') : `${t('dash.audit.batch', { op: t('pc.op.' + m[1]) })}${m[2] ? ' · ' + m[2] : ''}`;
    if (a === 'plan:bind') return t('dash.audit.bind'); // 只改所属套餐(也可能是移出套餐),额度和到期不动
    if ((m = /^plan:(renew|extend):(.*)$/.exec(a))) return `${t('pc.op.' + m[1])} · ${m[2]}`;
    if ((m = /^plan:(replace|stack|prorate):(.*)$/.exec(a))) return `${t('dash.audit.planChange', { mode: t('pc.mode.' + m[1]) })} → ${m[2]}`;
  }
  const whole = has('audit.v.' + a.replace(/:/g, '.'));
  if (whole) return whole;
  if ((m = /^batch:(.+)$/.exec(a))) return t('dash.audit.batch', { op: verb(m[1]) });
  if ((m = /^([a-z-]+):(.+)$/.exec(a))) return `${verb(m[1])} · ${key === 'ops' && m[1] === 'run' ? (has('ops.task.' + m[2] + '.title') || m[2]) : m[2]}`;
  return verb(a);
}
