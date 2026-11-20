// 界面偏好(用户 11-21):侧栏里隐藏的栏目、简洁模式。存在面板设置里(uiNavHidden / uiConcise),换浏览器、换电脑都一样;
// 主机容灾接任后跟着走(ha/settings.go 归全局)。只管主面板:代理面板拿不到这两项设置,永远是完整的侧栏和详细说明。
//   「有的用户可能有的栏目用不到，在设置里增加一个功能可隐藏栏目 点击轻量弹窗 可勾选隐藏左边的栏目（只是前端隐藏，设置不支持隐藏）」
//   「增加一个切换详细/简洁模式」
// 这个模块不 import 别的:前端测试的桩环境里直接用真的。

// NAV_LOCKED 不能隐藏的栏目:藏了就没地方改回来
export const NAV_LOCKED = 'settings';

// navHidden 现在隐藏的栏目(settings 传 state.settings);写法同后端 normNavHidden:小写字母数字、逗号分隔,去掉「设置」
export function navHidden(settings) {
  const out = new Set();
  for (const x of String((settings || {}).uiNavHidden || '').split(',')) {
    const id = x.trim().toLowerCase();
    if (id && id !== NAV_LOCKED && /^[a-z0-9]+$/.test(id)) out.add(id);
  }
  return out;
}

// normNavHidden 存进设置的写法:按侧栏顺序(order 传全部栏目),只留认得的,去掉「设置」
export function normNavHidden(ids, order) {
  const set = new Set([...ids].filter(id => id !== NAV_LOCKED));
  return order.filter(id => set.has(id)).join(',');
}

// visibleNav 侧栏里露出来的栏目(按原顺序);全藏光了也至少留「设置」
export function visibleNav(order, settings) {
  const hide = navHidden(settings);
  return order.filter(id => !hide.has(id));
}

// isConcise 简洁模式开着没有(和后端开关同一个判法:true / 1 / on / yes)
export function isConcise(settings) {
  return ['true', '1', 'on', 'yes'].includes(String((settings || {}).uiConcise ?? '').trim().toLowerCase());
}

// applyConcise 整页切到简洁 / 详细:<html data-concise>,样式和「?」都看它
export function applyConcise(on) {
  if (typeof document === 'undefined') return;
  document.documentElement.toggleAttribute('data-concise', !!on);
}
