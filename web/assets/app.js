// 看板页面共用：接口请求、DOM、格式化、日期区间、颜色分配、图表样式（开发文档 §7）。
// 令牌名、模型名来自上游，一律用 textContent 写进页面，不拼 HTML。

// ---- 接口 ----

/** 请求看板接口；未登录时跳转登录页，并在登录后回到当前页面。 */
export async function api(path, params = {}) {
  const url = new URL(path, location.origin);
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') url.searchParams.set(k, v);
  }
  const res = await fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } });
  if (res.status === 401) {
    redirectToLogin();
    throw new Error('请先登录');
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) throw new Error((body && body.message) || `请求失败（${res.status}）`);
  return body;
}

export function redirectToLogin() {
  location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search);
}

export async function logout() {
  await fetch('/api/v1/logout', { method: 'POST', credentials: 'same-origin' }).catch(() => {});
  location.href = '/login';
}

// ---- DOM ----

/** 创建元素。text 用 textContent 写入；子节点里的字符串也按文本处理。 */
export function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'style') Object.assign(node.style, v);
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

export const $ = (id) => document.getElementById(id);

export function showError(err) {
  const box = $('error');
  if (!box) return;
  box.replaceChildren(el('span', { class: 'badge critical' }, el('span', { class: 'dot' }), '出错了：' + err.message));
  box.hidden = false;
}

export function clearError() {
  const box = $('error');
  if (box) box.hidden = true;
}

// ---- 格式化 ----

const SYMBOL = { CNY: '¥', USD: '$' };

/** 金额：不足 0.01 时显示 4 位小数；币种是 TOKENS / QUOTA 时按整数显示。 */
export function money(v, currency, digits) {
  if (!(currency in SYMBOL)) return int(v);
  const d = digits ?? (v !== 0 && Math.abs(v) < 0.01 ? 4 : 2);
  return SYMBOL[currency] + v.toLocaleString('zh-CN', { minimumFractionDigits: d, maximumFractionDigits: d });
}

export const int = (n) => Math.round(n).toLocaleString('zh-CN');

/** 大数缩写：1.2万、3.4亿。 */
export function compact(n) {
  const a = Math.abs(n);
  const trim = (x) => x.toLocaleString('zh-CN', { maximumFractionDigits: Math.abs(x) >= 100 ? 0 : Math.abs(x) >= 10 ? 1 : 2 });
  if (a >= 1e8) return trim(n / 1e8) + '亿';
  if (a >= 1e4) return trim(n / 1e4) + '万';
  return int(n);
}

export function pct(x) {
  if (!isFinite(x)) return '—';
  return (x * 100).toFixed(x !== 0 && x < 0.01 ? 2 : 1) + '%';
}

let timeZone = 'Asia/Shanghai';

/** 日期和时间按服务端配置的时区显示（server.timezone）。 */
export function setTimeZone(tz) {
  if (tz) timeZone = tz;
}

function parts(ts, opts) {
  return new Intl.DateTimeFormat('zh-CN', { timeZone, hour12: false, ...opts }).formatToParts(new Date(ts * 1000))
    .reduce((m, p) => ({ ...m, [p.type]: p.value }), {});
}

export function dateTime(ts) {
  if (!ts) return '—';
  const p = parts(ts, { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}`;
}

export function clock(ts) {
  if (!ts) return '—';
  const p = parts(ts, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
  return `${p.month}-${p.day} ${p.hour}:${p.minute}`;
}

export function dateOnly(ts) {
  if (!ts) return '—';
  const p = parts(ts, { year: 'numeric', month: '2-digit', day: '2-digit' });
  return `${p.year}-${p.month}-${p.day}`;
}

// ---- 指标：金额 / 请求数 / Token（输入 + 输出） ----

export const METRICS = [
  { key: 'amount', label: '金额' },
  { key: 'requests', label: '请求数' },
  { key: 'tokens', label: 'Token' },
];

export function metricOf(u, metric) {
  if (metric === 'requests') return u.requests;
  if (metric === 'tokens') return u.prompt_tokens + u.completion_tokens;
  return u.amount;
}

/** 一条序列在当前指标下的取值数组。 */
export function lineValues(line, metric) {
  if (metric === 'requests') return line.requests;
  if (metric === 'tokens') return line.prompt_tokens.map((v, i) => v + line.completion_tokens[i]);
  return line.amount;
}

export function formatMetric(v, metric, currency, short = false) {
  if (metric === 'amount') return money(v, currency, short && v !== 0 && Math.abs(v) >= 100 ? 0 : undefined);
  return short ? compact(v) : int(v);
}

export function metricName(metric) {
  return metric === 'tokens' ? 'Token（输入 + 输出）' : METRICS.find((m) => m.key === metric).label;
}

// ---- 地址栏参数：日期区间和指标写进 URL，刷新、分享、进详情都保持 ----

export function readParams() {
  const p = new URLSearchParams(location.search);
  const metric = p.get('metric');
  return {
    start: p.get('start') || '',
    end: p.get('end') || '',
    metric: METRICS.some((m) => m.key === metric) ? metric : 'amount',
  };
}

/** 更新地址栏参数（空值删除），push 一条历史记录，返回键可以回到上一个区间。 */
export function writeParams(update) {
  const p = new URLSearchParams(location.search);
  for (const [k, v] of Object.entries(update)) {
    if (v === '' || v === null || v === undefined || (k === 'metric' && v === 'amount')) p.delete(k);
    else p.set(k, v);
  }
  const qs = p.toString();
  history.pushState(null, '', location.pathname + (qs ? '?' + qs : ''));
}

/** 保留日期区间和指标的查询串，用于页面之间跳转。 */
export function carryQuery(params, extra = {}) {
  const p = new URLSearchParams();
  if (params.start) p.set('start', params.start);
  if (params.end) p.set('end', params.end);
  if (params.metric && params.metric !== 'amount') p.set('metric', params.metric);
  for (const [k, v] of Object.entries(extra)) if (v !== '' && v != null) p.set(k, v);
  const qs = p.toString();
  return qs ? '?' + qs : '';
}

// ---- 日期：都用 YYYY-MM-DD 字符串按日历计算，不受浏览器时区影响 ----

export function addDays(day, n) {
  const d = new Date(day + 'T00:00:00Z');
  d.setUTCDate(d.getUTCDate() + n);
  return d.toISOString().slice(0, 10);
}

export function daysBetween(start, end) {
  return Math.round((Date.parse(end + 'T00:00:00Z') - Date.parse(start + 'T00:00:00Z')) / 86400000) + 1;
}

const monthStart = (day) => day.slice(0, 8) + '01';

export function presets(today) {
  const prevEnd = addDays(monthStart(today), -1);
  return [
    { label: '今天', start: today, end: today },
    { label: '昨天', start: addDays(today, -1), end: addDays(today, -1) },
    { label: '近 7 天', start: addDays(today, -6), end: today },
    { label: '近 30 天', start: addDays(today, -29), end: today },
    { label: '本月', start: monthStart(today), end: today },
    { label: '上月', start: monthStart(prevEnd), end: prevEnd },
  ];
}

const short = (day) => day.slice(5);

export function rangeLabel(range, today) {
  const hit = presets(today).find((p) => p.start === range.start && p.end === range.end);
  if (hit) return hit.label;
  return range.start === range.end ? range.start : `${range.start} ~ ${range.end}`;
}

export function rangeHint(range) {
  return range.start === range.end ? range.start : `${short(range.start)} ~ ${short(range.end)}`;
}

/**
 * 日期区间选择器：预设一行一个，选中项打勾；自定义区间在底部分隔线下，范围从 data_since 到今天。
 * onChange(start, end) 在选定后调用。
 */
export function rangePicker({ today, dataSince, range, onChange }) {
  const root = el('div', { class: 'range-picker' });
  const button = el('button', { class: 'btn', type: 'button', 'aria-haspopup': 'true', 'aria-expanded': 'false' },
    el('span', { text: '日期：' + rangeLabel(range, today) }), el('span', { class: 'muted', text: rangeHint(range) }), '▾');
  const panel = el('div', { class: 'range-panel', role: 'menu', hidden: true });
  const close = () => {
    panel.hidden = true;
    button.setAttribute('aria-expanded', 'false');
  };
  const pick = (start, end) => {
    close();
    if (start !== range.start || end !== range.end) onChange(start, end);
  };
  for (const p of presets(today)) {
    const selected = p.start === range.start && p.end === range.end;
    panel.append(el('button', { class: 'range-option', type: 'button', role: 'menuitemradio', 'aria-checked': String(selected),
      onclick: () => pick(p.start, p.end) },
    el('span', { class: 'check', text: selected ? '✓' : '' }), p.label, el('span', { class: 'hint', text: rangeHint(p) })));
  }
  const min = dataSince || '';
  const startInput = el('input', { type: 'date', value: range.start, min, max: today });
  const endInput = el('input', { type: 'date', value: range.end, min, max: today });
  const error = el('div', { class: 'range-error' });
  panel.append(el('div', { class: 'range-custom' },
    el('label', {}, '开始', startInput),
    el('label', {}, '结束', endInput),
    error,
    el('div', { class: 'actions' },
      el('button', { class: 'btn btn-primary', type: 'button', onclick: () => {
        const s = startInput.value, e = endInput.value;
        if (!s || !e) return (error.textContent = '请选择开始和结束日期');
        if (s > e) return (error.textContent = '开始日期不能晚于结束日期');
        if (daysBetween(s, e) > 400) return (error.textContent = '日期区间最长 400 天');
        pick(s, e);
      } }, '应用'))));
  if (dataSince) panel.append(el('div', { class: 'small muted', text: `最早可查 ${dataSince}` }));

  button.addEventListener('click', () => {
    panel.hidden = !panel.hidden;
    button.setAttribute('aria-expanded', String(!panel.hidden));
  });
  document.addEventListener('click', (e) => {
    if (!root.contains(e.target)) close();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') close();
  });
  root.append(button, panel);
  return root;
}

/**
 * 分段切换按钮。options: [{key, label, disabled?, title?}]。当前选中项由按钮组自己记着，
 * 调用方切换后可以不重建它（比如「图 / 表」只换显示）。
 */
export function segmented(options, value, onChange, label) {
  const root = el('span', { class: 'segmented', role: 'group', 'aria-label': label || '' });
  let current = value;
  const buttons = options.map((o) => el('button', { type: 'button', 'aria-pressed': String(o.key === value),
    disabled: !!o.disabled, title: o.title, onclick: () => {
      if (o.key === current) return;
      current = o.key;
      buttons.forEach((b, i) => b.setAttribute('aria-pressed', String(options[i].key === current)));
      onChange(o.key);
    } }, o.label));
  root.append(...buttons);
  if (!label) return root;
  return el('span', {}, el('span', { class: 'segmented-label', text: label }), root);
}

// ---- 状态：图标 + 文字，不只靠颜色（状态色只用于真正表示好坏的状态） ----

const STATUS = {
  enabled: ['good', '启用'],
  disabled: ['', '禁用'],
  expired: ['critical', '已过期'],
  exhausted: ['serious', '额度用尽'],
  deleted: ['', '已删除'],
  unattributed: ['', '未归属'],
  unknown: ['', '未知'],
};

export function statusBadge(status) {
  const [cls, label] = STATUS[status] || STATUS.unknown;
  return el('span', { class: 'badge ' + cls }, el('span', { class: 'dot' }), label);
}

export function badge(level, text) {
  return el('span', { class: 'badge ' + level }, el('span', { class: 'dot' }), text);
}

// ---- 颜色：按实体固定，不按名次（dataviz） ----

export function palette() {
  const cs = getComputedStyle(document.documentElement);
  const v = (name) => cs.getPropertyValue(name).trim();
  return {
    series: [1, 2, 3, 4, 5, 6, 7, 8].map((i) => v(`--series-${i}`)),
    other: v('--series-other'),
    surface: v('--surface-1'),
    grid: v('--grid'),
    axis: v('--axis'),
    text: v('--text-primary'),
    text2: v('--text-secondary'),
    muted: v('--text-muted'),
    hover: v('--hover'),
    font: v('--font'),
  };
}

// dataviz 校验脚本（validate_palette.js）逐对算出的、不能在图里相邻的色位对（从 0 数；浅色或深色任一模式下
// 正常视力 ΔE < 15，或色盲模拟 ΔE 落在失败区间）。色板按 1～8 的顺序相邻时都合格，跳着用时要避开这些对。
const BAD_NEIGHBORS = new Set([[0, 6], [1, 3], [1, 4], [1, 5], [1, 7], [2, 4], [2, 5], [3, 7], [4, 7]]
  .map(([a, b]) => a * 8 + b));
const badPair = (a, b) => BAD_NEIGHBORS.has(Math.min(a, b) * 8 + Math.max(a, b));

/**
 * 令牌的首选色位：未删除的令牌按累计消费从高到低依次占色位（用得最多的拿色板前几位），已删除的和「未归属」排在后面。
 * 累计消费不随日期区间变，令牌的颜色也就不随区间变。
 */
export function tokenSlotOrder(tokens) {
  const used = (t) => t.used_total ?? 0;
  const byUse = (a, b) => used(b) - used(a) || a.id - b.id;
  const live = tokens.filter((t) => t.id !== 0 && t.status !== 'deleted').sort(byUse);
  const rest = tokens.filter((t) => t.id !== 0 && t.status === 'deleted').sort(byUse);
  const order = new Map([...live, ...rest].map((t, i) => [t.id, i]));
  return (id) => (order.has(id) ? order.get(id) : order.size);
}

/** 模型的首选色位：按名字哈希，同一个模型在不同区间里颜色不变。 */
export function nameSlot(name) {
  let h = 0;
  for (const ch of name) h = (h * 31 + ch.codePointAt(0)) >>> 0;
  return h;
}

/** 按首选色位分配，被占用时顺延到下一个空位，同一视图里不会有两个实体同色。返回 key → 色位。 */
export function assignSlots(keys, preferred) {
  const used = new Set();
  const slots = new Map();
  for (const k of keys) {
    let slot = preferred(k) % 8;
    for (let i = 0; i < 8 && used.has(slot); i++) slot = (slot + 1) % 8;
    used.add(slot);
    slots.set(k, slot);
  }
  return slots;
}

/** 在所有排列里找相邻颜色都分得清的顺序（最多 5 个，120 种），同样好时尽量保持原顺序。 */
function arrange(keys, slots, circular) {
  let best = null;
  const score = (arr) => {
    let bad = 0;
    for (let i = 0; i + 1 < arr.length; i++) if (badPair(slots.get(arr[i]), slots.get(arr[i + 1]))) bad++;
    if (circular && arr.length > 2 && badPair(slots.get(arr[arr.length - 1]), slots.get(arr[0]))) bad++;
    const drift = arr.reduce((sum, k, i) => sum + Math.abs(i - keys.indexOf(k)), 0);
    if (!best || bad * 1000 + drift < best.bad * 1000 + best.drift) best = { order: arr.slice(), bad, drift };
  };
  const permute = (arr, l) => {
    if (l === arr.length) return score(arr);
    for (let i = l; i < arr.length; i++) {
      [arr[l], arr[i]] = [arr[i], arr[l]];
      permute(arr, l + 1);
      [arr[l], arr[i]] = [arr[i], arr[l]];
    }
  };
  permute(keys.slice(), 0);
  return best || { order: [], bad: 0 };
}

/**
 * 给一个视图里要着色的实体（最多 5 个，按名次排好）定色位和排列顺序（堆叠、图例、饼图共用）：
 * 先按首选色位分配，再找相邻都分得清的顺序；找不到时改用色板前几位按名次排（色板相邻顺序已校验过）。
 * circular 为真时首尾也算相邻（饼图没有「其他」块时）。「其他」用灰色，放在最后，和谁相邻都可以。
 */
export function colorPlan(keys, preferred, { circular = false } = {}) {
  let slots = assignSlots(keys, preferred);
  let best = arrange(keys, slots, circular);
  if (best.bad > 0) {
    slots = new Map(keys.map((k, i) => [k, i]));
    best = { order: keys.slice() };
  }
  return { slots, order: best.order };
}

// ---- 图表 ----

const forcedColors = () => matchMedia('(forced-colors: active)').matches;

/** 公共样式：系统无衬线字体、细网格线（1px 实线）、悬停提示用页面的 DOM 和样式。 */
export function baseOption(pal) {
  return {
    animationDuration: 300,
    textStyle: { fontFamily: pal.font, color: pal.text2 },
    // 强制颜色（高对比度）模式下打开图案填充，颜色不是唯一的区分手段。
    aria: { enabled: true, decal: { show: forcedColors() } },
    tooltip: {
      confine: true,
      backgroundColor: pal.surface,
      borderColor: pal.grid,
      borderWidth: 1,
      padding: [8, 10],
      textStyle: { color: pal.text, fontFamily: pal.font },
      extraCssText: 'border-radius: 8px; box-shadow: 0 4px 16px rgba(0,0,0,0.12);',
    },
  };
}

export function axisStyle(pal) {
  return {
    axisLine: { lineStyle: { color: pal.axis, width: 1 } },
    axisTick: { show: false },
    axisLabel: { color: pal.muted, fontFamily: pal.font },
    splitLine: { lineStyle: { color: pal.grid, width: 1, type: 'solid' } },
  };
}

/** 悬停提示：数值在前（加粗）、名称在后，每行用一条短线标识序列颜色。 */
export function tooltipNode(title, rows, total) {
  const root = el('div', { class: 'tip' }, el('div', { class: 'tip-title', text: title }));
  for (const r of rows) {
    root.append(el('div', { class: 'tip-row' },
      r.color ? el('span', { class: 'tip-key', style: { background: r.color } }) : null,
      el('span', { class: 'tip-value', text: r.value }),
      el('span', { class: 'tip-name', text: r.name })));
  }
  if (total) {
    root.append(el('div', { class: 'tip-row tip-total' },
      el('span', { class: 'tip-value', text: total.value }), el('span', { class: 'tip-name', text: total.name })));
  }
  return root;
}

/**
 * 创建图表并跟随容器大小变化。容器被隐藏（切到表格）时是 0×0，不跟着缩：画面留着，切回来不用重画；
 * 隐藏期间更新数据也照常画在原来的尺寸上。尺寸没变时也不重画。
 */
export function mountChart(node) {
  const chart = window.echarts.init(node, null, { renderer: 'canvas' });
  new ResizeObserver(() => {
    const w = node.clientWidth;
    const h = node.clientHeight;
    if (w && h && (w !== chart.getWidth() || h !== chart.getHeight())) chart.resize();
  }).observe(node);
  return chart;
}

/** 系统切换深色 / 浅色或强制颜色时重画（颜色取自 CSS 变量）。 */
export function onThemeChange(cb) {
  for (const q of ['(prefers-color-scheme: dark)', '(forced-colors: active)']) {
    matchMedia(q).addEventListener('change', cb);
  }
}

/** 表格：columns: [{label, num?, get(row) → 文本或节点, sortKey?}]；rows 按顺序输出。 */
export function table({ columns, rows, footer, onRowClick, sort, onSort, className }) {
  const head = el('tr', {}, columns.map((c) => {
    const sortable = c.sortKey && onSort;
    const arrow = sortable && sort && sort.key === c.sortKey ? (sort.dir < 0 ? ' ↓' : ' ↑') : '';
    return el('th', { class: [c.num ? 'num' : '', sortable ? 'sortable' : ''].join(' ').trim(),
      onclick: sortable ? () => onSort(c.sortKey) : null, 'aria-sort': arrow ? (sort.dir < 0 ? 'descending' : 'ascending') : null },
    c.label, arrow ? el('span', { class: 'arrow', text: arrow }) : null);
  }));
  const body = el('tbody', {}, rows.map((row) => el('tr', {
    class: onRowClick && row ? 'clickable' : '',
    tabindex: onRowClick ? '0' : null,
    onclick: onRowClick ? () => onRowClick(row) : null,
    onkeydown: onRowClick ? (e) => e.key === 'Enter' && onRowClick(row) : null,
  }, columns.map((c) => el('td', { class: c.num ? 'num' : '' }, c.get(row))))));
  const foot = footer ? el('tfoot', {}, el('tr', {}, columns.map((c, i) => el('td', { class: c.num ? 'num' : '' },
    footer[i] ?? '')))) : null;
  return el('table', { class: className }, el('thead', {}, head), body, foot);
}

export function swatch(color) {
  return el('span', { class: 'swatch', style: { background: color }, 'aria-hidden': 'true' });
}
