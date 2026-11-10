// 落地页 logo:主面板 设置 → 订阅页(接口 sub-logo)与代理面板 账号 → 订阅页(接口 self/logo)共用。
// 选好的图先在浏览器里用 canvas 缩到 480×160 以内、导出成 PNG(超过 64 KB 依次退到 360×120、240×80)再上传:
// 顺带去掉 EXIF 定位、SVG 里的脚本和外链,动图只留第一帧。服务端只收 PNG / JPEG / GIF,自己还会再校验一遍。
// 上传 / 恢复默认立即生效,和分组的「保存」按钮无关。
import { get, post, del } from './api.js';
import { t } from './i18n.js';
import { esc, toast, confirm, registerActions, fmtBytes } from './ui.js';

const MAX_FILE = 5 << 20;  // 原图上限:再大的图缩下来也没意义,先拦掉免得卡住页面
const MAX_LOGO = 64 << 10; // 和服务端一致
const SIZES = [[480, 160], [360, 120], [240, 80]];
const SVG_SIDE = 960;      // SVG 一律先按长边这么大栅格化(矢量图放大不糊),再缩到目标尺寸

const loadImage = url => new Promise((resolve, reject) => {
  const img = new Image();
  img.onload = () => resolve(img);
  img.onerror = () => reject(new Error(t('set.logoBad')));
  img.src = url;
});

const blobToDataURL = blob => new Promise((resolve, reject) => {
  const r = new FileReader();
  r.onload = () => resolve(r.result);
  r.onerror = () => reject(new Error(t('set.logoBad')));
  r.readAsDataURL(blob);
});

// svgURL 把 SVG 的宽高改写成长边 SVG_SIDE(比例取自原来写的宽高,没写就取 viewBox),返回可加载的地址。
// 只有 viewBox、或宽高写成百分比的,Firefox 画不到 canvas 上;写了 24×24 这种小尺寸的,按原样栅格化放到落地页上会糊。
// 矢量图放大不损失清晰度,所以一律先按大尺寸画,再和位图一样缩到 480×160 以内。
async function svgURL(file) {
  const doc = new DOMParser().parseFromString(await file.text(), 'image/svg+xml');
  const svg = doc.documentElement;
  if (!svg || svg.localName !== 'svg' || doc.getElementsByTagName('parsererror').length) throw new Error(t('set.logoBad'));
  const px = v => (/^\s*[\d.]+(px)?\s*$/i.test(v || '') ? parseFloat(v) : NaN);
  let w = px(svg.getAttribute('width')), h = px(svg.getAttribute('height'));
  const vb = (svg.getAttribute('viewBox') || '').trim().split(/[\s,]+/).map(Number);
  const hasVB = vb.length === 4 && vb.every(Number.isFinite) && vb[2] > 0 && vb[3] > 0;
  if (!(w > 0 && h > 0)) {
    if (hasVB) {
      w = vb[2]; h = vb[3];
    } else {
      w = 300; h = 150; // 浏览器对没写尺寸的 SVG 也是按 300×150 算
    }
  }
  // 没有 viewBox 时按原来的画布补一个:改大宽高时内容跟着放大,而不是只多出空白
  if (!hasVB) svg.setAttribute('viewBox', `0 0 ${w} ${h}`);
  const k = SVG_SIDE / Math.max(w, h);
  svg.setAttribute('width', String(Math.max(1, Math.round(w * k))));
  svg.setAttribute('height', String(Math.max(1, Math.round(h * k))));
  const text = new XMLSerializer().serializeToString(doc);
  return URL.createObjectURL(new Blob([text], { type: 'image/svg+xml' }));
}

// toPNG 画到 w×h 的画布上导出 PNG;缩小超过一半时先逐级减半,边缘不起锯齿。
function toPNG(img, sw, sh, w, h) {
  let src = img, cw = sw, ch = sh;
  while (cw / 2 >= w && ch / 2 >= h) {
    const c = document.createElement('canvas');
    c.width = Math.round(cw / 2); c.height = Math.round(ch / 2);
    c.getContext('2d').drawImage(src, 0, 0, c.width, c.height);
    src = c; cw = c.width; ch = c.height;
  }
  const c = document.createElement('canvas');
  c.width = w; c.height = h;
  const ctx = c.getContext('2d');
  ctx.imageSmoothingQuality = 'high';
  ctx.drawImage(src, 0, 0, w, h);
  return new Promise((resolve, reject) => {
    try { c.toBlob(b => (b ? resolve(b) : reject(new Error(t('set.logoBad')))), 'image/png'); }
    catch (e) { reject(e && e.name === 'SecurityError' ? new Error(t('set.logoSvgTainted')) : e); } // SVG 带 foreignObject 等:画布被污染
  });
}

// prepLogo 把用户选的文件变成要上传的 data URI(PNG)。
export async function prepLogo(file) {
  if (!file) throw new Error(t('set.logoBad'));
  if (file.size > MAX_FILE) throw new Error(t('set.logoFileTooBig'));
  const isSVG = file.type === 'image/svg+xml' || /\.svg$/i.test(file.name || '');
  const url = isSVG ? await svgURL(file) : URL.createObjectURL(file);
  try {
    const img = await loadImage(url);
    const w0 = img.naturalWidth, h0 = img.naturalHeight;
    if (!w0 || !h0) throw new Error(t('set.logoBad'));
    for (const [mw, mh] of SIZES) {
      const k = Math.min(1, mw / w0, mh / h0); // 只缩不放
      const blob = await toPNG(img, w0, h0, Math.max(1, Math.round(w0 * k)), Math.max(1, Math.round(h0 * k)));
      if (blob.size <= MAX_LOGO) return await blobToDataURL(blob);
    }
    throw new Error(t('set.logoTooBig'));
  } finally {
    URL.revokeObjectURL(url);
  }
}

// prefix → { path, inherit };inherit 为真是代理面板(没设时跟随主面板的)
const bound = {};

// logoBlock 设置卡片里的一块:预览、上传 / 更换、恢复默认。readonly 是副机(随主机同步,不能改)。
export function logoBlock(prefix, { readonly = false } = {}) {
  return `<div class="logo-field">
    <span class="label">${esc(t('set.subPageLogo'))}</span>
    <div class="logo-row">
      <div class="logo-preview"><img id="${prefix}-img" alt="" hidden><span id="${prefix}-none" class="muted small"></span></div>
      ${readonly ? '' : `<div class="row">
        <button class="btn sm" data-act="logo.pick" data-id="${prefix}">${esc(t('set.logoUpload'))}</button>
        <button class="btn sm ghost" data-act="logo.reset" data-id="${prefix}" id="${prefix}-reset" hidden>${esc(t('set.logoReset'))}</button>
      </div>
      <input type="file" id="${prefix}-file" accept="image/png,image/jpeg,image/gif,image/webp,image/svg+xml,.svg" data-change="logo.file" data-id="${prefix}" hidden>`}
    </div>
    <span class="help">${esc(readonly ? t('set.logoNode') : t('set.logoHelp'))}</span>
  </div>`;
}

function show(prefix, v) {
  const img = document.getElementById(prefix + '-img'), none = document.getElementById(prefix + '-none');
  if (!img || !none) return; // 页面已经切走
  const reset = document.getElementById(prefix + '-reset');
  const on = !!(v && v.set && v.data);
  const inherit = bound[prefix] && bound[prefix].inherit;
  if (on) {
    img.src = v.data;
    img.title = `${v.width}×${v.height} · ${fmtBytes(v.size, 1)}`;
  } else if (!inherit) {
    img.src = 'logo.svg'; // 主面板没设:落地页用的就是内置这张
    img.title = '';
  }
  img.hidden = !on && inherit;
  none.textContent = on ? '' : t(inherit ? 'acct.logoInherit' : 'set.logoDefault');
  none.hidden = on;
  if (reset) reset.hidden = !on;
}

// bindLogo 卡片画好之后调用:读当前的 logo 显示出来,记下这一块对应的接口。
export async function bindLogo(prefix, path, { inherit = false } = {}) {
  bound[prefix] = { path, inherit };
  let v = { set: false };
  try { v = await get(path); } catch { /* 读不到就按没设显示 */ }
  show(prefix, v);
}

registerActions({
  'logo.pick': id => {
    const f = document.getElementById(id + '-file');
    if (f) { f.value = ''; f.click(); }
  },
  'logo.file': async (id, el) => {
    const b = bound[id], file = el.files && el.files[0];
    if (!b || !file) return;
    try {
      const data = await prepLogo(file);
      show(id, await post(b.path, { data }));
      toast(t('set.logoSaved'), 'ok');
    } catch (e) { toast(e.message, 'err'); }
    finally { el.value = ''; }
  },
  'logo.reset': async id => {
    const b = bound[id];
    if (!b || !await confirm(t('set.logoResetConfirm'))) return;
    try { show(id, await del(b.path)); toast(t('set.logoCleared'), 'ok'); }
    catch (e) { toast(e.message, 'err'); }
  },
});
