// 令牌详情页（开发文档 §7）：基本信息、区间指标、趋势（按天 / 按小时，可切成表格）、模型占比和分布表、最近请求。
// 日期区间和指标沿用总览页带过来的参数；颜色和总览页一致。
import {
  $, METRICS, api, axisStyle, baseOption, carryQuery, clearError, colorPlan, compact, dateOnly, dateTime, daysBetween, el,
  formatMetric, int, lineValues, logout, metricName, metricOf, money, mountChart, nameSlot, onThemeChange, palette, pct,
  rangeLabel, rangePicker, readParams, segmented, setTimeZone, showError, statusBadge, swatch, table, tokenSlotOrder,
  tooltipNode, writeParams,
} from './app.js';

const TOP_MODELS = 5; // 饼图单独显示的模型数，其余合并为「其他」
const PAGE_SIZE = 20;
const MAX_HOUR_DAYS = 7;

const id = Number(location.pathname.split('/').pop());
const state = {
  params: readParams(),
  bucket: 'day',
  view: 'chart',
  page: 1,
  model: '',
};

let trendChart;
let modelChart;

init();

function init() {
  $('logout').addEventListener('click', logout);
  if (!Number.isInteger(id) || id < 0) {
    showError(new Error('令牌 id 不对'));
    return;
  }
  trendChart = mountChart($('trend-chart'));
  modelChart = mountChart($('model-chart'));
  window.addEventListener('popstate', () => {
    state.params = readParams();
    state.page = 1;
    load();
  });
  onThemeChange(render);
  load();
}

async function load() {
  document.body.classList.add('loading');
  try {
    const q = { start: state.params.start, end: state.params.end };
    const [summary, token, tokens] = await Promise.all([
      api('/api/v1/summary'), api(`/api/v1/tokens/${id}`), api('/api/v1/tokens', q),
    ]);
    setTimeZone(summary.timezone);
    const range = tokens.range;
    // 按小时最长 7 天：区间变长时退回按天。
    if (state.bucket === 'hour' && daysBetween(range.start, range.end) > MAX_HOUR_DAYS) state.bucket = 'day';
    const [series, models] = await Promise.all([
      api('/api/v1/usage/series', { ...range, bucket: state.bucket, token_id: id }),
      api('/api/v1/usage/models', { ...range, token_id: id }),
    ]);
    Object.assign(state, { summary, token, tokens, series, models });
    await loadLogs();
    clearError();
    render();
  } catch (err) {
    showError(err);
  } finally {
    document.body.classList.remove('loading');
  }
}

async function loadLogs() {
  state.logs = await api(`/api/v1/tokens/${id}/logs`, {
    ...state.tokens.range, page: state.page, page_size: PAGE_SIZE, model: state.model,
  });
}

function setRange(start, end) {
  writeParams({ start, end });
  state.params = readParams();
  state.page = 1;
  load();
}

function render() {
  if (!state.token) return;
  const pal = palette();
  // 令牌颜色和总览页同一区间里的一致：总览页给前 5 名定色位（见 colorPlan），不在前 5 名的用它的首选色位。
  const order = tokenSlotOrder(state.tokens.items);
  const used = state.tokens.items.filter((t) => t.usage.requests > 0)
    .sort((a, b) => b.usage.amount - a.usage.amount || a.id - b.id).map((t) => t.id);
  const plan = colorPlan(used.slice(0, TOP_MODELS), order, { circular: used.length <= TOP_MODELS });
  const color = pal.series[plan.slots.has(id) ? plan.slots.get(id) : order(id) % 8];
  const items = state.models.items; // 已按金额排序
  const mplan = colorPlan(items.slice(0, TOP_MODELS).map((m) => m.model), nameSlot, { circular: items.length <= TOP_MODELS });
  const modelColor = (name) => (mplan.slots.has(name) ? pal.series[mplan.slots.get(name)] : pal.other);
  document.title = `${state.token.name} · 令牌用量`;
  renderHeader();
  renderInfo();
  renderKPIs();
  renderTrend(pal, color);
  renderModels(pal, mplan, modelColor);
  renderLogs();
}

function renderHeader() {
  const { summary, tokens, params, token } = state;
  $('back').href = '/' + carryQuery({ ...tokens.range, metric: params.metric });
  $('title').replaceChildren(token.name, ' ', statusBadge(token.status));
  $('filters').replaceChildren(
    rangePicker({ today: summary.today, dataSince: summary.data_since, range: tokens.range, onChange: setRange }),
    segmented(METRICS, params.metric, (metric) => {
      writeParams({ metric });
      state.params = readParams();
      render();
    }, '指标'),
    el('span', { class: 'spacer' }),
    el('a', { class: 'btn', href: '/api/v1/export.csv' + carryQuery(tokens.range, { token_id: id }), download: '' }, '导出 CSV'),
  );
}

function renderInfo() {
  const t = state.token;
  const cur = t.currency;
  const field = (label, value) => el('div', {}, el('dt', { text: label }), el('dd', {}, value));
  const limits = t.model_limits.length ? t.model_limits.join('、') : '不限';
  $('info').replaceChildren(el('dl', { class: 'info-grid' },
    field('Key', t.key_hint || '—'),
    field('分组', t.group || '—'),
    field('剩余额度', t.unlimited ? '无限额度' : t.remain === null ? '—' : money(t.remain, cur)),
    field('累计消费', t.used_total === null ? '—' : money(t.used_total, cur)),
    field('过期时间', t.expires_at ? dateTime(t.expires_at) : '永不过期'),
    field('模型限制', limits),
    field('最后使用', t.last_used_at ? dateTime(t.last_used_at) : '—'),
    field('创建时间', t.created_at ? dateOnly(t.created_at) : '—'),
    t.deleted_at ? field('删除时间（发现）', dateTime(t.deleted_at)) : null,
  ));
}

function totals() {
  const s = state.series.series[0];
  const sum = (arr) => (arr || []).reduce((a, b) => a + b, 0);
  return {
    amount: sum(s && s.amount), requests: sum(s && s.requests),
    prompt_tokens: sum(s && s.prompt_tokens), completion_tokens: sum(s && s.completion_tokens),
  };
}

function renderKPIs() {
  const { tokens, summary, series } = state;
  const cur = series.currency;
  const t = totals();
  const range = tokens.range;
  const end = range.end < summary.today ? range.end : summary.today;
  const days = Math.max(daysBetween(range.start, end), 1);
  const kpi = (label, value, sub, hero) => el('div', { class: 'kpi' + (hero ? ' hero' : '') },
    el('div', { class: 'label', text: label }), el('div', { class: 'value', text: value }),
    sub ? el('div', { class: 'sub', text: sub }) : null);
  $('kpis').replaceChildren(
    kpi('区间消费', money(t.amount, cur), `${rangeLabel(range, summary.today)} · ${range.start} ~ ${range.end}`, true),
    kpi('请求数', int(t.requests)),
    kpi('Token', compact(t.prompt_tokens + t.completion_tokens), `输入 ${compact(t.prompt_tokens)} · 输出 ${compact(t.completion_tokens)}`),
    kpi('日均消费', money(t.amount / days, cur), `按 ${days} 天`),
  );
}

function renderTrend(pal, color) {
  const { series, tokens } = state;
  const metric = state.params.metric;
  const cur = series.currency;
  const line = series.series[0];
  const values = line ? lineValues(line, metric) : series.buckets.map(() => 0);
  const hourly = series.bucket === 'hour';
  const longRange = daysBetween(tokens.range.start, tokens.range.end) > MAX_HOUR_DAYS;
  const labels = series.buckets.map((b) => (hourly ? (series.buckets.length > 24 ? b.slice(5) : b.slice(11)) : b.slice(5)));
  const empty = values.every((v) => v === 0);
  $('trend-sub').textContent = metricName(metric);
  $('trend-controls').replaceChildren(
    segmented([{ key: 'day', label: '按天' }, {
      key: 'hour', label: '按小时', disabled: longRange, title: longRange ? `日期区间不超过 ${MAX_HOUR_DAYS} 天时可按小时查看` : null,
    }], state.bucket, async (b) => {
      state.bucket = b;
      try {
        state.series = await api('/api/v1/usage/series', { ...tokens.range, bucket: b, token_id: id });
        render();
      } catch (err) {
        showError(err);
      }
    }),
    segmented([{ key: 'chart', label: '图' }, { key: 'table', label: '表' }], state.view, (v) => {
      state.view = v;
      renderTrendView();
    }),
  );

  // 单条序列：2px 折线 + 10% 的面积，只在最后一个点放 8px 圆点（带 2px 背景色描边）；十字线跟随指针。
  const last = values.length - 1;
  const axis = axisStyle(pal);
  trendChart.setOption({
    ...baseOption(pal),
    grid: { left: 4, right: 16, top: 16, bottom: 4, containLabel: true },
    tooltip: {
      ...baseOption(pal).tooltip,
      trigger: 'axis',
      axisPointer: { type: 'line', lineStyle: { color: pal.axis, width: 1 } },
      formatter: (params) => {
        const i = params[0].dataIndex;
        return tooltipNode(series.buckets[i], [{ color, value: formatMetric(values[i], metric, cur), name: metricName(metric) }]);
      },
    },
    xAxis: { type: 'category', boundaryGap: false, data: labels, ...axis, splitLine: { show: false },
      axisLabel: { ...axis.axisLabel, hideOverlap: true } },
    yAxis: { type: 'value', ...axis, axisLine: { show: false },
      axisLabel: { ...axis.axisLabel, formatter: (v) => formatMetric(v, metric, cur, true) } },
    graphic: empty ? [{ type: 'text', left: 'center', top: 'middle', style: { text: '这段时间没有用量', fill: pal.muted, font: `13px ${pal.font}` } }] : [],
    series: [{
      type: 'line', name: metricName(metric), data: values.map((v, i) => (i === last ? {
        value: v, symbol: 'circle', symbolSize: 8, itemStyle: { color, borderColor: pal.surface, borderWidth: 2 },
      } : { value: v, symbol: 'none' })),
      showSymbol: true, lineStyle: { color, width: 2, cap: 'round', join: 'round' },
      itemStyle: { color }, areaStyle: { color, opacity: 0.1 },
      emphasis: { disabled: true },
    }],
  }, true);

  renderTrendView();
  const sum = (k) => (line ? line[k].reduce((a, b) => a + b, 0) : 0);
  $('trend-table').replaceChildren(table({
    columns: [
      { label: hourly ? '时间' : '日期', get: (i) => series.buckets[i] },
      { label: '金额', num: true, get: (i) => money(line ? line.amount[i] : 0, cur) },
      { label: '请求数', num: true, get: (i) => int(line ? line.requests[i] : 0) },
      { label: '输入', num: true, get: (i) => compact(line ? line.prompt_tokens[i] : 0) },
      { label: '输出', num: true, get: (i) => compact(line ? line.completion_tokens[i] : 0) },
      { label: '缓存读', num: true, get: (i) => compact(line ? line.cache_read_tokens[i] : 0) },
    ],
    rows: series.buckets.map((_, i) => i),
    footer: ['合计', money(sum('amount'), cur), int(sum('requests')), compact(sum('prompt_tokens')),
      compact(sum('completion_tokens')), compact(sum('cache_read_tokens'))],
  }));
}

// 图 / 表切换只换显示：图表和表格在 renderTrend 里都已画好，图表隐藏时也不缩（见 mountChart），切回来不用重画。
function renderTrendView() {
  $('trend-chart').hidden = state.view !== 'chart';
  $('trend-table').hidden = state.view !== 'table';
}

function renderModels(pal, mplan, modelColor) {
  const { models } = state;
  const metric = state.params.metric;
  const cur = models.currency;
  const total = metricOf(models.total, metric);
  const f = (v) => formatMetric(v, metric, cur);
  // 前 5 个模型按颜色方案的顺序排（相邻的颜色分得清），「其他」是其余模型实际相加。
  const byName = new Map(models.items.map((m) => [m.model, m]));
  const rows = mplan.order.map((name) => ({ name: name || '（未知模型）', value: metricOf(byName.get(name), metric), color: modelColor(name) }));
  const rest = models.items.slice(TOP_MODELS).reduce((s, m) => s + metricOf(m, metric), 0);
  if (rest > 0) rows.push({ name: '其他', value: rest, color: pal.other });
  const slices = rows.filter((r) => r.value > 0);

  modelChart.setOption({
    ...baseOption(pal),
    tooltip: {
      ...baseOption(pal).tooltip, trigger: 'item',
      formatter: (p) => tooltipNode(p.name, [{ color: slices[p.dataIndex].color, value: f(p.value), name: pct(p.value / total) }]),
    },
    graphic: [{
      type: 'group', left: 'center', top: 'middle',
      children: [
        { type: 'text', style: { text: total > 0 ? formatMetric(total, metric, cur, true) : '无用量', x: 0, y: 0, textAlign: 'center', fill: pal.text, font: `600 18px ${pal.font}` } },
        { type: 'text', style: { text: '合计', x: 0, y: 26, textAlign: 'center', fill: pal.muted, font: `12px ${pal.font}` } },
      ],
    }],
    series: [{
      type: 'pie', radius: ['58%', '84%'], label: { show: false }, labelLine: { show: false },
      itemStyle: { borderColor: pal.surface, borderWidth: 2 }, emphasis: { scale: true, scaleSize: 4 },
      data: slices.map((r) => ({ name: r.name, value: r.value, itemStyle: { color: r.color } })),
    }],
  }, true);

  // 模型分布表：全部模型，兼作饼图的图例和表格视图。
  const t = models.total;
  $('model-table').replaceChildren(models.items.length ? table({
    columns: [
      { label: '模型', get: (m) => el('span', {}, swatch(modelColor(m.model)), m.model || '（未知模型）') },
      { label: '次数', num: true, get: (m) => int(m.requests) },
      { label: '输入', num: true, get: (m) => compact(m.prompt_tokens) },
      { label: '输出', num: true, get: (m) => compact(m.completion_tokens) },
      { label: '缓存读', num: true, get: (m) => compact(m.cache_read_tokens) },
      { label: '金额', num: true, get: (m) => money(m.amount, cur) },
      { label: `占比（${METRICS.find((m) => m.key === metric).label}）`, num: true, get: (m) => pct(total > 0 ? metricOf(m, metric) / total : 0) },
    ],
    rows: models.items,
    footer: ['合计', int(t.requests), compact(t.prompt_tokens), compact(t.completion_tokens), compact(t.cache_read_tokens),
      money(t.amount, cur), total > 0 ? '100%' : '—'],
  }) : el('div', { class: 'empty', text: '这段时间没有用量' }));
}

function renderLogs() {
  const { logs, models } = state;
  const cur = logs.currency;
  const pages = Math.max(Math.ceil(logs.total / logs.page_size), 1);
  const select = el('select', { class: 'search', 'aria-label': '按模型筛选' },
    el('option', { value: '', text: '全部模型' }),
    models.items.map((m) => el('option', { value: m.model, text: m.model || '（未知模型）', selected: m.model === state.model })));
  if (state.model && !models.items.some((m) => m.model === state.model)) {
    select.append(el('option', { value: state.model, text: state.model, selected: true }));
  }
  select.addEventListener('change', async () => {
    state.model = select.value;
    state.page = 1;
    await reloadLogs();
  });
  $('logs-controls').replaceChildren(select);
  $('logs-table').replaceChildren(logs.items.length ? table({
    columns: [
      { label: '时间', get: (r) => dateTime(r.created_at) },
      { label: '模型', get: (r) => r.model || '—' },
      { label: '输入', num: true, get: (r) => int(r.prompt_tokens) },
      { label: '输出', num: true, get: (r) => int(r.completion_tokens) },
      { label: '缓存读', num: true, get: (r) => int(r.cache_read_tokens) },
      { label: '耗时', num: true, get: (r) => r.use_time + ' 秒' },
      { label: '首字', num: true, get: (r) => (r.frt_ms ? (r.frt_ms / 1000).toFixed(2) + ' 秒' : '—') },
      { label: '流式', get: (r) => (r.is_stream ? '是' : '否') },
      { label: '金额', num: true, get: (r) => money(r.amount, cur) },
    ],
    rows: logs.items,
  }) : el('div', { class: 'empty', text: '这段时间没有请求（明细只保留 90 天）' }));
  const go = async (page) => {
    state.page = page;
    await reloadLogs();
  };
  $('logs-pager').replaceChildren(
    el('span', { text: `共 ${int(logs.total)} 条 · 第 ${logs.page} / ${pages} 页` }),
    el('button', { class: 'btn', type: 'button', disabled: logs.page <= 1, onclick: () => go(logs.page - 1) }, '上一页'),
    el('button', { class: 'btn', type: 'button', disabled: logs.page >= pages, onclick: () => go(logs.page + 1) }, '下一页'),
  );
}

async function reloadLogs() {
  try {
    await loadLogs();
    renderLogs();
  } catch (err) {
    showError(err);
  }
}
