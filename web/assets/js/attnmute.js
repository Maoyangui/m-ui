// 概况「需要处理」里各类提醒的开关:设置页「概况提醒」一块,存在设置 attnMute 里(关掉的那几类,逗号分隔,默认都开)。
// 用户 11-18:「概览页的故障提醒 在设置单独加一个区域 可以设置勾选哪些不提醒 现在的很多提醒已经有反馈比较烦了」。
// 关掉的只是概况(以及证书、大陆连通那两条顶部常驻提示)不再提,各自的页面照常显示;Telegram 通知另有自己的开关。
// 面板自己的数据取不到 / 过期了的那几条不在这里:那时概况本身已经不可信,不能关。
// 这个模块不 import 别的(设置由调用方传进来):前端测试的桩环境里纯模块用真的,提示条的判断测的才是真逻辑。

// role:只在主机(master)或只在副机(node)上会出现的;没写的两边都有。group:设置页里分三小块
export const ATTN_CATS = [
  { id: 'nodes', group: 'server', role: 'master' }, { id: 'quota', group: 'server' }, { id: 'reload', group: 'server' },
  { id: 'disk', group: 'server' }, { id: 'fw', group: 'server' }, { id: 'ha', group: 'server' }, { id: 'node', group: 'server', role: 'node' },
  { id: 'upstreams', group: 'net' }, { id: 'reach', group: 'net', role: 'master' }, { id: 'v6', group: 'net', role: 'master' }, { id: 'checks', group: 'net' },
  { id: 'cert', group: 'other' }, { id: 'resellers', group: 'other', role: 'master' }, { id: 'backup', group: 'other', role: 'master' }, { id: 'rejected', group: 'other' },
];
export const ATTN_GROUPS = ['server', 'net', 'other'];

// attnCatsFor 这个角色的面板上会出现的那几类
export const attnCatsFor = isNode => ATTN_CATS.filter(c => !c.role || c.role === (isNode ? 'node' : 'master'));

// normMutes 设置里存的写法:认得的类别,按上面的顺序,逗号分隔
export function normMutes(v) {
  const set = new Set(String(v || '').split(',').map(x => x.trim()).filter(Boolean));
  return ATTN_CATS.map(c => c.id).filter(id => set.has(id)).join(',');
}

// attnMuted 现在关掉的类别(settings 传 state.settings)
export function attnMuted(settings) {
  return new Set(normMutes((settings || {}).attnMute).split(',').filter(Boolean));
}
