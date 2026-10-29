// 总览页（开发文档 §7）：指标卡、同步状态、按天柱状图（可切成表格）、占比饼图、令牌表。
// 日期区间和指标在同一行，作用于下面全部内容；所有数字都从同一个区间汇总，彼此一致。
import {
  $, METRICS, api, axisStyle, badge, baseOption, carryQuery, clearError, clock, colorPlan, compact, daysBetween, el,
  formatMetric, int, lineValues, logout, metricName, metricOf, money, mountChart, nameSlot, onThemeChange, palette, pct,
  rangeLabel, rangePicker, readParams, segmented, setTimeZone, showError, statusBadge, swatch, table, tokenColorPlan,
  tooltipNode, writeParams, dateTime,
} from './app.js';

// 「按模型」饼图单独显示的模型数，其余合并为「其他」。令牌不合并：柱状图和「按令牌」饼图里每个令牌都单独一段 / 一块。
const TOP_MODELS = 5;

const state = {
  params: readParams(),
  summary: null,
  tokens: null,
  series: null,
  models: null,
  modelsRange: '',
  pieMode: 'token',
  view: 'chart',
  search: '',
  sort: { key: 'amount', dir: -1 },
};

let barChart;
let pieChart;
let pieRows = [];
let barTableJob = null; // 还没建的按天明细表，见 scheduleBarTable

// 没有 requestIdleCallback 的浏览器（Safari）退回 setTimeout。
const whenIdle = (cb) => (window.requestIdleCallback ? window.requestIdleCallback(cb) : setTimeout(cb, 1));
const cancelIdle = (id) => (window.cancelIdleCallback ? window.cancelIdleCallback(id) : clearTimeout(id));

init();

function init() {
  $('logout').addEventListener('click', logout);
  $('search').addEventListener('input', (e) => {
    state.search = e.target.value.trim().toLowerCase();
    renderTokenTable();
  });
  barChart = mountChart($('bar-chart'));
  pieChart = mountChart($('pie-chart'));
  // 整列都是点击目标：点某一天的柱子，整页切到这一天。
  barChart.getZr().on('click', (e) => {
    const pt = [e.offsetX, e.offsetY];
    if (!state.series || !barChart.containPixel('grid', pt)) return;
    const i = Math.round(barChart.convertFromPixel({ gridIndex: 0 }, pt)[0]);
    const day = state.series.buckets[i];
    if (day) setRange(day, day);
  });
  pieChart.on('click', (p) => {
    const row = pieRows[p.dataIndex];
    if (row && row.id !== undefined) openToken(row.id);
  });
  window.addEventListener('popstate', () => {
    state.params = readParams();
    load();
  });
  onThemeChange(render);
  load();
}

async function load() {
  document.body.classList.add('loading');
  try {
    const q = { start: state.params.start, end: state.params.end };
    const [summary, tokens, series] = await Promise.all([
      api('/api/v1/summary'),
      api('/api/v1/tokens', q),
      api('/api/v1/usage/series', { ...q, top: 'all' }),
    ]);
    Object.assign(state, { summary, tokens, series });
    setTimeZone(summary.timezone);
    if (state.pieMode === 'model') await loadModels();
    clearError();
    render();
  } catch (err) {
    showError(err);
  } finally {
    document.body.classList.remove('loading');
  }
}

async function loadModels() {
  const key = state.tokens.range.start + '/' + state.tokens.range.end;
  if (state.modelsRange === key) return;
  state.models = await api('/api/v1/usage/models', state.tokens.range);
  state.modelsRange = key;
}

function setRange(start, end) {
  writeParams({ start, end });
  state.params = readParams();
  load();
}

function setMetric(metric) {
  writeParams({ metric });
  state.params = readParams();
  render();
}

function openToken(id) {
  location.href = `/tokens/${id}` + carryQuery({ ...state.tokens.range, metric: state.params.metric });
}

function render() {
  if (!state.tokens) return;
  const pal = palette();
  // 令牌颜色跟着令牌走（按累计消费定首选色位），排列顺序保证相邻的颜色分得清；柱状图、饼图、令牌表用同一份。
  // 区间内消费前 8 名着色，其余灰色（序列已按区间金额从高到低排）。
  const plan = tokenColorPlan(state.series.series.map((l) => l.token_id), state.tokens.items);
  const colorOf = (id) => (plan.slots.has(id) ? pal.series[plan.slots.get(id)] : pal.other);
  renderFilters();
  renderStatus();
  renderKPIs();
  renderBar(pal, plan, colorOf);
  renderPie(pal, plan, colorOf);
  renderTokenTable(colorOf);
}

function renderFilters() {
  const { summary, tokens, params } = state;
  $('filters').replaceChildren(
    rangePicker({ today: summary.today, dataSince: summary.data_since, range: tokens.range, onChange: setRange }),
    segmented(METRICS, params.metric, setMetric, '指标'),
    el('span', { class: 'spacer' }),
    el('a', { class: 'btn', href: '/api/v1/export.csv' + carryQuery(tokens.range), download: '' }, '导出 CSV'),
  );
}

function renderStatus() {
  const { sync, reconcile, currency } = state.summary;
  const items = [];
  if (sync.error) items.push(badge('critical', '同步异常：' + sync.error));
  if (sync.backfilling) {
    const p = Math.round(sync.backfill_progress * 100);
    items.push(badge('', `首次回填中 ${p}%`), el('span', { class: 'meter', role: 'progressbar', 'aria-valuenow': p,
      'aria-valuemin': 0, 'aria-valuemax': 100 }, el('span', { style: { width: p + '%' } })));
  }
  if (reconcile && reconcile.warn) {
    const snap = reconcile.snapshot === null ? '' : `，令牌快照 ${money(reconcile.snapshot, currency)}`;
    items.push(badge('warning', `${reconcile.day} 对账不一致：上游 ${money(reconcile.upstream, currency)}，本地 ${money(reconcile.local, currency)}${snap}`));
  }
  if (!sync.error && !sync.backfilling) items.push(badge('good', '同步正常'));
  items.push(el('span', { class: 'muted', text: '数据截止 ' + clock(sync.synced_at) }));
  $('status').replaceChildren(...items);
}

function kpi(label, value, sub, hero) {
  return el('div', { class: 'kpi' + (hero ? ' hero' : '') },
    el('div', { class: 'label', text: label }), el('div', { class: 'value', text: value }),
    sub ? el('div', { class: 'sub', text: sub }) : null);
}

function renderKPIs() {
  const { tokens, summary } = state;
  const cur = tokens.currency;
  const t = tokens.total;
  const range = tokens.range;
  const end = range.end < summary.today ? range.end : summary.today;
  const days = Math.max(daysBetween(range.start, end), 1);
  const acc = summary.account;
  $('kpis').replaceChildren(
    kpi('区间消费', money(t.amount, cur), `${rangeLabel(range, summary.today)} · ${range.start} ~ ${range.end}`, true),
    kpi('请求数', int(t.requests)),
    kpi('Token', compact(t.prompt_tokens + t.completion_tokens), `输入 ${compact(t.prompt_tokens)} · 输出 ${compact(t.completion_tokens)}`),
    kpi('日均消费', money(t.amount / days, cur), `按 ${days} 天`),
    kpi('账户余额', acc ? money(acc.balance, cur) : '—', acc ? '更新于 ' + clock(acc.updated_at) : '还没有同步'),
    kpi('启用令牌', `${summary.tokens.enabled} / ${summary.tokens.total}`, '启用 / 全部（不含已删除）'),
  );
}

// 堆叠顺序（从下往上，图例和表格同序）：着色的令牌按颜色方案的顺序（相邻的颜色分得清），
// 灰色的令牌按区间金额从高到低接在上面。换指标时顺序不变，每个令牌的位置不跳。
function orderedLines(plan) {
  const lines = state.series.series;
  const byId = new Map(lines.map((l) => [l.token_id, l]));
  return [...plan.order.map((id) => byId.get(id)), ...lines.filter((l) => !plan.slots.has(l.token_id))];
}

function uniqueNames(lines) {
  const seen = new Map();
  for (const l of lines) seen.set(l.name, (seen.get(l.name) || 0) + 1);
  return lines.map((l) => (seen.get(l.name) > 1 ? `${l.name} #${l.token_id}` : l.name));
}

function renderBar(pal, plan, colorOf) {
  const { series } = state;
  const metric = state.params.metric;
  const cur = series.currency;
  const lines = orderedLines(plan);
  const names = uniqueNames(lines);
  const values = lines.map((l) => lineValues(l, metric));
  const colors = lines.map((l) => colorOf(l.token_id));
  const n = series.buckets.length;
  // 每一列最上面的非零段才有 4px 圆角，底部是直角。
  const topOf = series.buckets.map((_, i) => {
    for (let k = lines.length - 1; k >= 0; k--) if (values[k][i] > 0) return k;
    return -1;
  });
  const empty = values.every((v) => v.every((x) => x === 0));
  const colored = plan.order.length; // orderedLines 把着色的令牌排在前面
  const gray = lines.length - colored;
  $('bar-sub').textContent = metricName(metric) + (gray > 0 ? ` · 灰色是其余 ${gray} 个令牌，悬停柱子或切到「表」查看` : '');

  const axis = axisStyle(pal);
  barChart.setOption({
    ...baseOption(pal),
    color: colors,
    grid: { left: 4, right: 12, top: 36, bottom: 4, containLabel: true },
    // 图例只列着色的令牌：灰色的色块都一样，列进图例也认不出，靠副标题、悬停提示和表格。
    // 名字长时一行放不下，翻页查看；点图例可以隐藏 / 显示某个令牌。
    legend: lines.length > 1 ? {
      type: 'scroll', top: 0, left: 0, right: 0, icon: 'rect', itemWidth: 10, itemHeight: 10, itemGap: 14,
      data: names.slice(0, colored),
      textStyle: { color: pal.text2, fontFamily: pal.font }, inactiveColor: pal.grid,
      pageIconColor: pal.text2, pageIconInactiveColor: pal.grid, pageTextStyle: { color: pal.muted, fontFamily: pal.font },
    } : { show: false },
    // 悬停一天：列出这天每个有用量的令牌，按数值从高到低；令牌多时分几列，每列约 200px 宽。
    tooltip: {
      ...baseOption(pal).tooltip,
      trigger: 'axis',
      axisPointer: { type: 'shadow', shadowStyle: { color: pal.hover } },
      formatter: (params) => {
        const i = params[0].dataIndex;
        const rows = lines.map((l, k) => ({ k, v: values[k][i] })).filter((r) => r.v > 0).sort((a, b) => b.v - a.v)
          .map((r) => ({ color: colors[r.k], value: formatMetric(r.v, metric, cur), name: names[r.k] }));
        const total = values.reduce((s, v) => s + v[i], 0);
        return tooltipNode(series.buckets[i], rows.length ? rows : [{ value: '无用量', name: '' }],
          { value: formatMetric(total, metric, cur), name: rows.length > 1 ? `合计（${rows.length} 个令牌）` : '合计' },
          Math.max(1, Math.floor(barChart.getWidth() / 200)));
      },
    },
    xAxis: {
      type: 'category', data: series.buckets.map((b) => (n > 1 ? b.slice(5) : b)),
      ...axis, splitLine: { show: false }, axisLabel: { ...axis.axisLabel, hideOverlap: true },
    },
    yAxis: {
      type: 'value', ...axis, axisLine: { show: false },
      axisLabel: { ...axis.axisLabel, formatter: (v) => formatMetric(v, metric, cur, true) },
    },
    graphic: empty ? [{ type: 'text', left: 'center', top: 'middle', style: { text: '这段时间没有用量', fill: pal.muted, font: `13px ${pal.font}` } }] : [],
    series: lines.map((l, k) => ({
      type: 'bar', name: names[k], stack: 'usage', barMaxWidth: 24, barCategoryGap: '30%',
      // 段与段之间用背景色隔开 2px（每段各 1px），不画描边。
      itemStyle: { color: colors[k], borderColor: pal.surface, borderWidth: 1 },
      emphasis: { focus: 'none' },
      data: values[k].map((v, i) => (topOf[i] === k ? { value: v, itemStyle: { borderRadius: [4, 4, 0, 0] } } : v)),
    })),
  }, true);

  scheduleBarTable(() => renderBarTable(lines, names, values, colors));
  $('bar-view').replaceChildren(segmented([{ key: 'chart', label: '图' }, { key: 'table', label: '表' }], state.view, (v) => {
    state.view = v;
    renderBarView();
  }, '视图'));
  renderBarView();
}

// 按天明细表每个令牌一列，令牌多时有上千个单元格。看图时等浏览器空闲再建，不拖慢重画；
// 正在看表、或者切到表时还没建好，就马上建。
function scheduleBarTable(build) {
  if (barTableJob) cancelIdle(barTableJob.id);
  barTableJob = { build, id: whenIdle(flushBarTable) };
  if (state.view === 'table') flushBarTable();
}

function flushBarTable() {
  if (!barTableJob) return;
  const { build, id } = barTableJob;
  barTableJob = null;
  cancelIdle(id);
  build();
}

// 图 / 表切换只换显示：图表隐藏时也不缩（见 mountChart），切回来不用重画；表格一般已经在空闲时建好。
function renderBarView() {
  if (state.view === 'table') flushBarTable();
  $('bar-chart').hidden = state.view !== 'chart';
  $('bar-table').hidden = state.view !== 'table';
}

// 表格视图：行是日期，列是图里的各序列，最后一行 / 一列是合计。
function renderBarTable(lines, names, values, colors) {
  const { series } = state;
  const metric = state.params.metric;
  const cur = series.currency;
  const f = (v) => formatMetric(v, metric, cur);
  const rowTotal = (i) => values.reduce((s, v) => s + v[i], 0);
  const columns = [
    { label: '日期', get: (i) => series.buckets[i] },
    ...lines.map((l, k) => ({ label: el('span', {}, swatch(colors[k]), names[k]), num: true, get: (i) => f(values[k][i]) })),
    { label: '合计', num: true, get: (i) => f(rowTotal(i)) },
  ];
  const sums = values.map((v) => v.reduce((s, x) => s + x, 0));
  const footer = ['合计', ...sums.map(f), f(sums.reduce((s, x) => s + x, 0))];
  $('bar-table').replaceChildren(table({ columns, rows: series.buckets.map((_, i) => i), footer, className: 'matrix' }));
}

function renderPie(pal, plan, colorOf) {
  const metric = state.params.metric;
  const cur = state.tokens.currency;
  $('pie-mode').replaceChildren(segmented([{ key: 'token', label: '按令牌' }, { key: 'model', label: '按模型' }],
    state.pieMode, async (mode) => {
      state.pieMode = mode;
      try {
        if (mode === 'model') await loadModels();
        render();
      } catch (err) {
        showError(err);
      }
    }));

  let rows;
  let total;
  if (state.pieMode === 'token') {
    // 每个令牌单独一块，不合并「其他」：着色的令牌和柱状图同一批、同一套颜色和顺序，灰色的按当前指标从高到低接在后面。
    const byId = new Map(state.tokens.items.map((t) => [t.id, t]));
    total = metricOf(state.tokens.total, metric);
    const row = (id) => {
      const item = byId.get(id);
      return { id, name: item ? item.name : `#${id}`, value: item ? metricOf(item.usage, metric) : 0, color: colorOf(id) };
    };
    const tail = state.tokens.items.filter((t) => !plan.slots.has(t.id)).map((t) => row(t.id))
      .sort((a, b) => b.value - a.value || a.id - b.id);
    rows = [...plan.order.map(row), ...tail];
  } else {
    const items = state.models.items; // 已按金额排序
    total = metricOf(state.models.total, metric);
    const top = items.slice(0, TOP_MODELS);
    const mplan = colorPlan(top.map((m) => m.model), nameSlot, { circular: items.length <= TOP_MODELS });
    const byName = new Map(top.map((m) => [m.model, m]));
    rows = mplan.order.map((name) => ({ name: name || '（未知模型）', value: metricOf(byName.get(name), metric),
      color: pal.series[mplan.slots.get(name)] }));
    // 模型的「其他」是其余模型实际相加，不用合计相减。
    const rest = items.slice(TOP_MODELS).reduce((s, m) => s + metricOf(m, metric), 0);
    if (rest > 0) rows.push({ name: '其他', value: rest, color: pal.other });
  }
  rows = rows.filter((r) => r.value > 0);
  pieRows = rows;

  const f = (v) => formatMetric(v, metric, cur);
  pieChart.setOption({
    ...baseOption(pal),
    tooltip: {
      ...baseOption(pal).tooltip,
      trigger: 'item',
      formatter: (p) => tooltipNode(p.name, [{ color: rows[p.dataIndex].color, value: f(p.value), name: pct(p.value / total) }]),
    },
    graphic: [{
      type: 'group', left: 'center', top: 'middle',
      children: [
        { type: 'text', style: { text: total > 0 ? formatMetric(total, metric, cur, true) : '无用量', x: 0, y: 0, textAlign: 'center', fill: pal.text, font: `600 18px ${pal.font}` } },
        { type: 'text', style: { text: '合计', x: 0, y: 26, textAlign: 'center', fill: pal.muted, font: `12px ${pal.font}` } },
      ],
    }],
    series: [{
      type: 'pie', radius: ['58%', '84%'], avoidLabelOverlap: true, label: { show: false }, labelLine: { show: false },
      // 扇区之间用背景色隔开 2px。
      itemStyle: { borderColor: pal.surface, borderWidth: 2 },
      emphasis: { scale: true, scaleSize: 4 },
      cursor: state.pieMode === 'token' ? 'pointer' : 'default',
      data: rows.map((r) => ({ name: r.name, value: r.value, itemStyle: { color: r.color } })),
    }],
  }, true);

  // 图例兼表格视图：名称、数值、占比。按令牌时列出全部令牌（超出高度在表内滚动），点击进详情。
  const clickable = state.pieMode === 'token';
  $('pie-legend').replaceChildren(table({
    columns: [
      { label: state.pieMode === 'token' ? '令牌' : '模型', get: (r) => el('span', {}, swatch(r.color), r.name) },
      { label: metricName(metric), num: true, get: (r) => formatMetric(r.value, metric, cur, metric !== 'amount') },
      { label: '占比', num: true, get: (r) => pct(total > 0 ? r.value / total : 0) },
    ],
    rows,
    onRowClick: clickable ? (r) => r.id !== undefined && openToken(r.id) : null,
  }));
}

const SORTS = {
  name: (t) => t.name,
  amount: (t) => t.usage.amount,
  requests: (t) => t.usage.requests,
  prompt: (t) => t.usage.prompt_tokens,
  completion: (t) => t.usage.completion_tokens,
  cache: (t) => t.usage.cache_read_tokens,
  last: (t) => t.last_used_at || 0,
};

function renderTokenTable(colorOf) {
  if (!state.tokens) return;
  if (colorOf) state.colorOf = colorOf;
  const { tokens, params, sort, search } = state;
  const cur = tokens.currency;
  const metric = params.metric;
  const total = metricOf(tokens.total, metric);
  const rows = tokens.items
    .filter((t) => !search || t.name.toLowerCase().includes(search) || (t.key_hint || '').toLowerCase().includes(search))
    .sort((a, b) => {
      const x = SORTS[sort.key](a), y = SORTS[sort.key](b);
      return (x < y ? -1 : x > y ? 1 : 0) * sort.dir || a.id - b.id;
    });
  $('token-sub').textContent = `${tokens.items.length} 个 · 点击查看详情`;
  const columns = [
    { label: '令牌', sortKey: 'name', get: (t) => el('span', { class: 'name-cell' }, swatch(state.colorOf(t.id)), t.name,
      t.key_hint ? el('span', { class: 'key', text: t.key_hint }) : null) },
    { label: '状态', get: (t) => statusBadge(t.status) },
    { label: '金额', num: true, sortKey: 'amount', get: (t) => money(t.usage.amount, cur) },
    { label: '请求数', num: true, sortKey: 'requests', get: (t) => int(t.usage.requests) },
    { label: '输入', num: true, sortKey: 'prompt', get: (t) => compact(t.usage.prompt_tokens) },
    { label: '输出', num: true, sortKey: 'completion', get: (t) => compact(t.usage.completion_tokens) },
    { label: '缓存读', num: true, sortKey: 'cache', get: (t) => compact(t.usage.cache_read_tokens) },
    { label: `占比（${METRICS.find((m) => m.key === metric).label}）`, num: true,
      get: (t) => pct(total > 0 ? metricOf(t.usage, metric) / total : 0) },
    { label: '剩余', num: true, get: (t) => (t.unlimited ? '无限额度' : t.remain === null ? '—' : money(t.remain, cur)) },
    { label: '最后使用', sortKey: 'last', get: (t) => (t.last_used_at ? dateTime(t.last_used_at) : '—') },
  ];
  const t = tokens.total;
  const footer = ['合计', '', money(t.amount, cur), int(t.requests), compact(t.prompt_tokens), compact(t.completion_tokens),
    compact(t.cache_read_tokens), total > 0 ? '100%' : '—', '', ''];
  $('token-table').replaceChildren(rows.length ? table({
    columns, rows, footer, sort, onRowClick: (row) => openToken(row.id),
    onSort: (key) => {
      state.sort = { key, dir: state.sort.key === key ? -state.sort.dir : key === 'name' ? 1 : -1 };
      renderTokenTable();
    },
  }) : el('div', { class: 'empty', text: search ? '没有匹配的令牌' : '还没有令牌' }));
}
