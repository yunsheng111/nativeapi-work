'use strict';
/* ── 状态 ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
let theme = localStorage.getItem(LS_THEME) || 'auto';   // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null, lastUID = '', sessData = null;
// 宿主当前登录的账号 uid，按客户端版本分开（host/current 拉取；空 = 未知/未登录）。
// 国内版/国际版是两个独立客户端，可同时各登录一个池内账号，互不覆盖。
let hostUIDs = { cn: '', global: '' };
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;    // 只在推送建不起来时使用的兜底轮询定时器
let localTimer = null;  // 本地时钟：推进倒计时这类随时间自变的显示，不发请求
let overviewAt = 0;     // 最近一次 overview 落地的本地时刻，用来把快照里的剩余秒数换算到当前
/* 监控视图状态：放文件头部而非监控块——go() 在脚本加载期就会执行（含 hash 直达
   监控页），let 声明若在后面会踩暂时性死区，让整个面板初始化报错。 */
let monRange = '24h', monTab = 'detail', monFilters = { uid: '', model: '', mode: '' }, monTimer = null;
let monEntries = [], monUidSig = '', monReloadTimer = null, monDdQuery = '';
/* 分页状态：monPage 从 1 起（1 = 最新一页），monTotal 是服务端返回的过滤后总数
   （翻页时用它收敛页码输入），monRenderedPage 是表体当前真实内容所属的页——取数
   失败时靠它把页码回滚，避免"页码前进、表体没变"。monSeenModels 跨页累积模型名，
   模型筛选下拉在 models_all 拉取失败时用它兜底，只认当前页会随翻页缩水。 */
let monPage = 1, monPageSize = 100, monTotal = 0, monRenderedPage = 1;
const monSeenModels = new Set();

const $ = id => document.getElementById(id);

/* ── 主题 ─────────────────────────────────────────────────────────── */
/* 两态翻转（浅/深），首次访问跟随系统偏好；点击总是切换可见外观，符合直觉。 */
function effTheme() {
  return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
  const eff = effTheme();
  document.documentElement.dataset.theme = eff;
  $('icoTheme').innerHTML = eff === 'light'
    ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
    : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
  $('btnTheme').title = eff === 'light' ? '切换到深色' : '切换到浅色';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
  theme = effTheme() === 'light' ? 'dark' : 'light';
  localStorage.setItem(LS_THEME, theme);
  applyTheme();
};
applyTheme();

/* ── 请求 ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
  const h = Object.assign({}, opts.headers || {});
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  if (opts.body) h['Content-Type'] = 'application/json';
  // cache:'no-store'：监控查询串里 to= 是分钟精度，同一分钟内的刷新 URL 相同，
  // 不禁缓存时 WebView2 会回缓存里的旧响应——SSE 触发的实时刷新就"慢一拍"。
  const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h, cache: 'no-store' }));
  if (r.status === 401) { openKey(); throw new Error('密钥无效或未填写'); }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
  return d;
}
function toast(msg, cls) {
  const el = document.createElement('div');
  el.className = 'tst ' + (cls || '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), 3600);
}
// copyField 复制输入框内容（API 地址 / 密钥等只读展示位）。
// 优先 Clipboard API；被权限策略拒绝时退化为全选内容，用户一个 Ctrl+C 即可，
// 而不是只弹一句"复制失败"却不给出路。
function copyField(el, okMsg) {
  const text = el.value.trim();
  if (!text) { toast('内容为空，无可复制', 'err'); return; }
  const fallback = () => {
    el.focus(); el.select();
    toast('复制失败，已选中内容，请按 Ctrl+C', 'err');
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(() => toast(okMsg, 'ok'), fallback);
  } else fallback();
}
// esc 文本/属性双安全转义。不能只用 div.innerHTML（它转义 <>& 但不转义引号），
// 否则字符串拼进 HTML 属性（如 title="uid: ..."）时引号可闭合属性并注入事件处理器。
// 显式替换 5 个字符：& < > " '（& 必须最先，避免二次转义）。
function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
function ago(iso) {
  if (!iso || iso.startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}
function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  return h ? h + '时' + String(m).padStart(2, '0') + '分' : m ? m + '分' + String(s).padStart(2, '0') + '秒' : s + '秒';
}

function formatTokenCount(tokens) {
  if (tokens == null || tokens === '') return '—';
  const n = Number(tokens);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return String(Math.round(n));
  const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
  let unit = units[0];
  for (const candidate of units) {
    if (n >= candidate[1]) unit = candidate;
  }
  let value = n / unit[1];
  let rounded = Number(value.toFixed(1));
  // 999999 → 1m，而不是 1000k；四舍五入后自动升级单位。
  const next = units[units.indexOf(unit) + 1];
  if (next && rounded >= 1000) {
    unit = next;
    value = n / unit[1];
    rounded = Number(value.toFixed(1));
  }
  return rounded + unit[0];
}

function formatLatency(ms) {
  if (ms == null || ms === '') return '—';
  const n = Number(ms);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
  if (rate == null || rate === '') return '—';
  const n = Number(rate);
  if (!Number.isFinite(n) || n < 0) return '—';
  return n.toFixed(1) + 'tok/s';
}

/* ── 密钥门 ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
  const v = $('keyInput').value.trim();
  if (!v) return;
  localStorage.setItem(LS_KEY, v);
  try {
    await api('overview');
    $('keyErr').hidden = true;
    $('keyVeil').classList.remove('on');
    start();
  } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── 路由 ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: '账号池', sessions: '会话', monitoring: '监控', packages: '积分构成', taskscenter: '任务中心', models: '模型与档位', config: '配置', logs: '运行日志' };
function go(v) {
  view = v;
  document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
  document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
  $('ttl').textContent = TITLES[v];
  if (v !== 'monitoring') stopMonTimer(); // 离开监控：收掉运行状态页签的 5s 轮询
  if (v === 'sessions') loadSessions();
  if (v === 'monitoring') loadMonitoring();
  if (v === 'models' && !$('mdBody').children.length) loadModels();
  if (v === 'config') loadConfig();
  if (v === 'logs') loadLogs();
  if (v === 'packages') loadPackages();
  if (v === 'taskscenter') { loadSchoolStatus(true); pollQueueOnce(true); }
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
go((location.hash || '#accounts').slice(1) in TITLES ? (location.hash || '#accounts').slice(1) : 'accounts');

/* ── 账号池 ───────────────────────────────────────────────────────── */
// 一次典型对话请求的 in_flight 窗口实测只有 ~1.4s（首字节 0.9s、总耗时 1.7s）。
// 池状态现在由服务端在变更瞬间推过来，窗口起点不会再被采样周期错过；last_used_at
// 是"这次尝试落在哪个号"的结构化时间戳（成功/失败都刷新），仍作为兜底：高亮在
// 请求结束、in_flight 归零之后还能靠它续亮一会儿，不至于一眨眼就灭。
const JUST_USED_MS = 6000;

function usedWithin(s, ms) {
  const t = Date.parse((s.token_usage || {}).last_used_at || '') || 0;
  return t > 0 && Date.now() - t < ms;
}

// coolParts 把冷却剩余时间换算到"此刻"。overview 是某一刻的快照，字段里的
// cool_remaining_sec 是快照那一刻的剩余秒数；快照之后本地时钟还在走，直接渲染
// 这个数会让倒计时停在拉取那一刻。熔断到期时刻 breaker_until 是绝对时间，可直接算。
function coolParts(s) {
  const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
  const hard = bl > 0 ? bl : 0;
  const soft = Math.max(0, (s.cool_remaining_sec || 0) - (Date.now() - overviewAt) / 1000);
  return { hard: hard, soft: soft, total: Math.max(hard, soft) };
}

// isInUse：该行是否显示绿色「使用中」。三个来源任一成立即可——
// 粘性会话绑定在此号 / 有在途请求 / 刚被调用过。
function isInUse(s) {
  return boundExact(s.uid) > 0 || (s.in_flight || 0) > 0 || usedWithin(s, JUST_USED_MS);
}

// lastAccKey 是上一次真正写入 DOM 的渲染输入指纹。刷新由事件驱动、可能来得较密
// （一个请求会触发 Acquire/Release 多次通知），输入没变就重建 tbody 会清掉鼠标
// hover 态、并把用户正按住的按钮换成新节点。
let lastAccKey = '';

function renderAccounts(list) {
  const tb = $('accBody');
  // 指纹覆盖全部渲染输入：账号字段 + 粘性绑定表 + 宿主账号 + 每行的高亮态 + 冷却秒数。
  // 后两项不能省：「刚被调用过」和冷却倒计时都会随时间自己翻转，而账号数据本身不变，
  // 少了它们本地时钟那一拍就会被指纹拦掉，倒计时会停住。
  const key = list.length
    ? JSON.stringify(list) + '|' + JSON.stringify(overviewData && overviewData.bound_uids) + '|' + hostUIDs.cn + '|' + hostUIDs.global +
      '|' + list.map(s => (isInUse(s) ? 1 : 0)).join('') +
      '|' + list.map(s => Math.ceil(coolParts(s).total)).join(',')
    : 'empty';
  if (key === lastAccKey) return;
  lastAccKey = key;
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>';
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  tb.innerHTML = list.map(s => {
    const coolP = coolParts(s);
    const dgRem = Math.max(0, (new Date(s.degrade_until || 0) - Date.now()) / 1000);
    // 连败降权（degrade_until）与冷却/熔断并列：total 取三者最远，kind 按主导维度归因。
    const cool = { hard: coolP.hard, soft: coolP.soft, dg: dgRem, total: Math.max(coolP.total, dgRem) };
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (s.locked) { cls = 'cool'; tag = '<span class="tag warn">已锁定</span>'; }
    else if (cool.total > 0) {
      cls = 'cool';
      const softish = Math.max(cool.soft, cool.dg);
      const kind = cool.hard > softish ? '熔断'
        : (cool.dg > 0 && cool.dg >= cool.soft ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool.total) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    // 状态徽章：冷却/熔断时把剩余时长挂在 title（列被 CSS 裁剪时悬浮可读全文）
    const tagTip = cls === 'cool' ? tag.replace(/<[^>]+>/g, '') : '';
    // uid 整串渲染，列宽不够时由 .id 的 CSS 省略号截断——宽窗口下能多看到几位，
    // 窄窗口退化成原来的 16 字符截断，不必在 JS 里猜列宽。
    const cred = s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits));
    const pct = s.credits_total > 0
      ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
      : Math.round((s.credits || 0) / maxCred * 100);
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），
    // 「为什么总选它」一眼可见（免费号垄断 / 单价排序）。
    let credTip = s.credits_total > 0 ? '剩余 ' + s.credits + ' / 总额 ' + s.credits_total + '（' + pct + '%）' : '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool.total > 0;
    // 使用中：粘性会话绑定在此号、或它正在处理请求。行整体绿色高亮；
    // "在用哪个账号"标在换号按钮位（点击仍可换号），状态列不重复出现。
    const boundN0 = boundExact(s.uid);
    const inFlight = s.in_flight || 0;
    const inUse = isInUse(s);
    const useTip = [
      boundN0 ? '有 ' + boundN0 + ' 个粘性会话绑定在此号' : '',
      inFlight ? '在途请求 ' + inFlight : '',
      (!boundN0 && !inFlight && usedWithin(s, JUST_USED_MS)) ? '刚被调用过' : '',
    ].filter(Boolean).join('；');
    // 换号按钮：禁用号本就不参与选号，换号无意义 → 渲染为禁用态占位而非省略。
    // 操作列是 4×2 网格，格子缺席会让后续按钮顺移、跨行对齐破掉。
    // 在用的号显示绿色「使用中 ✓」（仍可点击 = 换号把它推开），一眼看到当前在用哪个账号。
    const boundN = boundCount(s.uid);
    const ejectBtn = s.disabled
      ? '<button class="xs ghost" disabled title="账号已禁用，不参与选号，无需换号">换号</button>'
      : inUse
        ? '<button class="xs use" data-a="eject" data-u="' + esc(s.uid) + '" title="' + esc(useTip) + '；点击换号：把该号推开 5 分钟并解绑其会话">使用中 ✓</button>'
        : '<button class="xs ghost" data-a="eject" data-u="' + esc(s.uid) + '" title="把该号从选号中推开 5 分钟，并解绑它上面的 ' + boundN + ' 个会话">换号</button>';
    // 锁定/解锁：人工"我不想用它"（不被自动复活路径清除），与"禁用"（判死）并存。
    // 已锁定时按钮显示「解锁」；否则显示「锁定」。禁用号也给锁定按钮——两维度正交，
    // 用户可能既判死又锁定（解锁后仍禁用），面板应允许分别操作。
    const lockBtn = s.locked
      ? '<button class="xs primary" data-a="unlock" data-u="' + esc(s.uid) + '" title="解除人工锁定">解锁</button>'
      : '<button class="xs ghost" data-a="lock" data-u="' + esc(s.uid) + '" title="锁定后该号不再被选中；与「禁用」不同，锁定不会被签到/重登自动清除">锁定</button>';
    // 宿主切号：把该账号写入其 realm 对应客户端的登录态（会重启该客户端）。
    // 已是宿主的行不再给按钮，改为标记，避免重复操作。国内版/国际版是两个独立
    // 客户端，各自有自己的宿主，标记按账号 realm 匹配对应版本的登录号。
    const hostClient = s.realm === 'global' ? 'WorkBuddyAI 国际版客户端' : 'WorkBuddy 国内版客户端';
    const isHost = (s.realm === 'global' ? hostUIDs.global : hostUIDs.cn) === s.uid;
    const hostBtn = isHost
      ? '<span class="tag ok" title="该账号当前就是 ' + hostClient + ' 登录的账号">宿主</span>'
      : '<button class="xs ghost" data-a="host" data-u="' + esc(s.uid) + '" title="把该账号设为 ' + hostClient + ' 的登录账号（写入官方认证文件并重启该客户端，客户端当前会话会中断）">设为宿主</button>';
    return '<tr class="' + cls + (inUse ? ' inuse' : '') + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.recent ? ' <span class="tag accent" title="最近一次请求落在这个号">最近</span>' : '') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + '</div><div class="id">' + esc(s.uid) + '</div></td>' +
      '<td><div class="clip" title="' + esc(tagTip) + '">' + tag + '</div>' + note + '</td>' +
      '<td class="cred" title="' + credTip + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num in-flight-col">' + (s.in_flight || 0) + '</td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="num">' + (boundExact(s.uid) > 0 ? '<span class="tag accent" title="该号当前挂着的粘性会话数">' + boundExact(s.uid) + '</span>' : '<span class="tag mute">0</span>') + '</td>' +
      '<td class="c-acts"><div class="acts">' +
        ejectBtn +
        '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '">签到</button>' +
        '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '">余额</button>' +
        '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>' +
        lockBtn +
        hostBtn +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '" title="解冻：清除禁用与冷却，恢复参与选号">解冻</button>'
                : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '" title="禁用＝判死：该号不再参与选号，需手动解冻；与「锁定」的区别——禁用可被自动复活路径清除，锁定是人工避让、不会被签到/重登自动推翻">禁用</button>') +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</div></td></tr>';
  }).join('');
}

// boundCount 返回某账号当前挂着的粘性会话数（overview.bound_uids 派生；字段缺失时 1，
// 表示"至少可能有一个"——用 0 会让「换号」按钮的 title 显示"解绑 0 个会话"而实际有，
// 反而误导）。overviewData 未就绪时也返回 1（保守估计）。
function boundCount(uid) {
  const m = overviewData && overviewData.bound_uids;
  if (!m || m[uid] == null) return 1;
  return m[uid];
}

// boundExact 与 boundCount 不同：列显示用，缺失即 0——保守值 1 只适合"点击前提示影响面"，
// 用作列显示会凭空多出一个会话。
function boundExact(uid) {
  const m = overviewData && overviewData.bound_uids;
  return (m && m[uid]) || 0;
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    overviewAt = Date.now();   // 快照时刻：冷却倒计时从这一刻继续走
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    // disabled 计数含人工锁定号（pool 侧保证 total = healthy+cooling+disabled 恒等式）。
    // 若有锁定号，在数字后补一个小标记，让"不可用里有多少是我自己锁的"一眼可见。
    $('sDisabled').textContent = d.locked ? d.disabled + '（锁 ' + d.locked + '）' : d.disabled;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
  $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    $('sSticky').textContent = d.sticky_sessions;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    // 最近请求（B）：取各号 token_usage.last_used_at 的最大者。不用日志文本解析——
    // last_used_at 是每次尝试（含失败）都会刷新的结构化时间戳，与"这一跳用了谁"同源。
    const accts = d.accounts || [];
    let bestTs = 0;
    lastUID = '';
    accts.forEach(s => {
      const ts = Date.parse((s.token_usage || {}).last_used_at || '') || 0;
      if (ts > bestTs) { bestTs = ts; lastUID = s.uid; }
    });
    accts.forEach(s => { s.recent = s.uid === lastUID; });
    const la = accts.find(s => s.uid === lastUID);
    if (la && bestTs) {
      const nick = la.nickname || lastUID.slice(0, 8) + '…';
      $('sLastChat').textContent = nick + ' · ' + ago((la.token_usage || {}).last_used_at);
      $('sLastChat').title = '最近一次对话请求落在：' + nick + '（' + lastUID + '）';
    } else { $('sLastChat').textContent = '-'; }
    renderAccounts(d.accounts || []);
    loadHostCurrent(); // 异步：不阻塞 overview 渲染
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

// loadHostCurrent 拉取两个版本宿主的当前登录账号并更新统计位。静默失败（501 = 非
// Windows 或未启用；宿主状态不渲染成错误，只是保持 '-'）。
async function loadHostCurrent() {
  try {
    const r = await api('host/current');
    const fill = (elId, cur, inPool, realm) => {
      const client = realm === 'global' ? 'WorkBuddyAI（国际版）' : 'WorkBuddy（国内版）';
      $(elId).textContent = cur.file_exists ? ((cur.nickname || (cur.uid ? cur.uid.slice(0, 8) + '…' : '')) || '-') : '未登录';
      $(elId).style.color = inPool ? '' : 'var(--warn, #b8860b)';
      $(elId).title = cur.file_exists
        ? client + ' 当前登录：' + (cur.nickname || cur.uid || '-') + (inPool ? '（在账号池中）' : '（不在账号池中）')
        : client + ' 尚未登录（认证文件不存在）';
    };
    fill('sHostCn', r.current || {}, !!r.in_pool, 'cn');
    fill('sHostGlobal', r.current_global || {}, !!r.in_pool_global, 'global');
    hostUIDs.cn = (r.current || {}).file_exists ? ((r.current || {}).uid || '') : '';
    hostUIDs.global = (r.current_global || {}).file_exists ? ((r.current_global || {}).uid || '') : '';
    // 宿主 uid 变了要立即反映到账号行的「宿主」标记上：renderAccounts 有指纹拦截，
    // hostUIDs 不变时这次调用是空转，变了才重建 tbody。
    if (overviewData) renderAccounts(overviewData.accounts || []);
  } catch (e) { /* 501/网络错误：保持 '-'，不打扰 */ }
}

// hostSwitchConfirmText 「设为宿主」确认弹窗文案：按目标账号的 realm 提示会重启哪个
// 客户端（国内版/国际版是两个独立安装，只重启目标版本，另一版本不受影响）。
function hostSwitchConfirmText(uid) {
  const acc = ((overviewData || {}).accounts || []).find(s => s.uid === uid) || {};
  const client = acc.realm === 'global' ? 'WorkBuddyAI（国际版客户端）' : 'WorkBuddy（国内版客户端）';
  return '「设为宿主」会把该账号写入 ' + client + ' 的登录态文件，并关闭后重启该客户端——\n客户端里当前打开的会话会中断（包括本面板如果开在客户端里，重启后重新打开即可）。\n当前宿主登录态会先自动备份。确认切换？';
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
  if (a === 'eject' && !confirm('换号会把该账号从选号中推开 5 分钟，并解绑它上面的全部会话（这些会话下一轮会落到其他账号）。确认换号？')) return;
  if (a === 'lock' && !confirm('锁定后该账号不再被选中，且不会被签到/重登自动恢复，需手动解锁。确认锁定？')) return;
  if (a === 'host' && !confirm(hostSwitchConfirmText(u))) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'eject') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/eject', { method: 'POST' });
      toast('已换号：避让 ' + r.duration_sec + ' 秒，解绑 ' + r.unbound_sessions + ' 个会话', 'ok');
    } else if (a === 'lock') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/lock', { method: 'POST' });
      toast('已锁定' + (r.unbound_sessions ? '，解绑 ' + r.unbound_sessions + ' 个会话' : ''), 'ok');
    } else if (a === 'unlock') {
      await api('accounts/' + encodeURIComponent(u) + '/unlock', { method: 'POST' });
      toast('已解锁', 'ok');
    } else if (a === 'host') {
      const r = await api('host/switch', { method: 'POST', body: JSON.stringify({ uid: u }) });
      const res = r.result || {};
      toast('宿主已切换为该账号' + (res.launched_workbuddy ? '，对应客户端已重启' : '（请手动启动对应客户端）') + (res.backup ? '。备份：' + res.backup : ''), 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

$('btnForceSwitch').onclick = async () => {
  if (!confirm('一键换号会解绑全部粘性会话，让所有进行中的对话下一轮落到其他账号上。\n账号状态不变（不会禁用任何号）。确认执行？')) return;
  const b = $('btnForceSwitch');
  b.disabled = true;
  try {
    const r = await api('switch/force', { method: 'POST' });
    toast('已换号：解绑 ' + r.unbound_sessions + ' 个会话，当前可用 ' + r.available + ' 个账号', 'ok');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
};

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测数据是可选增强：拉取失败不影响模型列表本身
    const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
    const list = d.models || [];
    if (!list.length) { tb.innerHTML = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>'; return; }
    const probes = pr.probes || {};
    const probeKeys = Object.keys(probes);
    const probeOf = id => probes[id] || probes[probeKeys.find(k => k.endsWith(':' + id))];
    tb.innerHTML = list.map(m => {
      const eff = (m.supported_efforts || []).slice();
      if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
      const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
        : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
      // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
      const caps = [];
      if (m.is_default) caps.push('<span class="tag ok">默认</span>');
      if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
      if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
      if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
      const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
      const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
      return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
        '<td class="num">' + (m.credits ? esc(m.credits) : '—') + '</td>' +
        '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '<td class="efs" style="white-space:normal">' + effs + '</td>' +
        '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
        outCell(m, probeOf(m.id)) + '</tr>';
    }).join('');
    const hit = list.filter(m => probeOf(m.id)).length;
    $('mdNote').textContent = list.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
$('btnModels').onclick = loadModels;

/* ── 监控：四页签（请求明细 / 错误请求 / 用量分析 / 运行状态） ────── */
/* 明细/错误共用 chatlogs（时间窗与筛选由服务端承担，前端只组查询串）；
   分析走 usage_stats 聚合桶；运行状态走 metrics 负载指标 + overview 资源快照。
   筛选变更即时重拉；SSE/轮询触发的静默刷新（quiet）失败不弹 toast。 */
function pad2(n) { return String(n).padStart(2, '0'); }
// normModel 模型名展示统一带 realm 前缀：裸名等价于 cn（resolveModel 协议），
// 客户端有填裸名有填全名的，监控里统一回显成 cn:xxx / global:xxx 与模型列表一致。
function normModel(m) {
  const s = String(m == null ? '' : m).trim();
  if (!s || s === '-') return s;
  return /^(cn|global):/.test(s) ? s : 'cn:' + s;
}
// bareModel 剥掉 realm 前缀（匹配用：填不填前缀都能命中）。
function bareModel(m) { return String(m == null ? '' : m).replace(/^(cn|global):/, ''); }
// toLocalInput Date(ms) → 本地时间串（YYYY-MM-DDTHH:mm，datetime-local 同格式）
function toLocalInput(ms) {
  const d = new Date(ms);
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + 'T' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
// fmtRowTime 表格行时间：今天只显示时刻，跨天带日期
function fmtRowTime(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return '—';
  const hm = pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
  const now = new Date();
  return (d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate())
    ? hm : (d.getMonth() + 1) + '-' + d.getDate() + ' ' + hm;
}
// fmtFullTime 完整年月日时分秒，用于时间列的 title——列宽按最长跨天格式定死，
// 但浏览器缩放（Ctrl+滚轮）会等比放大字体，超出时仍能靠 title 读到完整时刻。
function fmtFullTime(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return '';
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' +
    pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
}
// acctName 行内 uid 是前 8 位短号：映射回账号昵称，映射不上显示原前缀
function acctName(uid8) {
  if (!uid8 || uid8 === '-') return '—';
  const accts = (overviewData && overviewData.accounts) || [];
  const a = accts.find(x => (x.uid || '').slice(0, uid8.length) === uid8);
  return a && a.nickname ? a.nickname + ' (' + uid8 + ')' : uid8;
}
// monWindow 时间范围 → from/to 本地时间串（datetime-local 同格式，服务端按本地时区解析）
function monWindow() {
  const now = Date.now();
  let fromMs;
  if (monRange === '1h') fromMs = now - 3600e3;
  else if (monRange === '6h') fromMs = now - 6 * 3600e3;
  else if (monRange === '24h') fromMs = now - 86400e3;
  else { const d = new Date(); d.setHours(0, 0, 0, 0); fromMs = d.getTime(); } // today = 本地零点
  return { from: toLocalInput(fromMs), to: toLocalInput(now) };
}

// monQS 组装 chatlogs/usage_stats 查询串；extra 追加 limit/offset/err_only 等附加参数
function monQS(extra) {
  const w = monWindow();
  const p = new URLSearchParams({ from: w.from, to: w.to });
  if (monFilters.uid) p.set('uid', monFilters.uid);
  if (monFilters.model) p.set('model', monFilters.model);
  if (monFilters.mode) p.set('mode', monFilters.mode);
  for (const k in (extra || {})) p.set(k, extra[k]);
  return p.toString();
}

// renderMonStats 顶部统计卡：detail/err 由 chatlogs 汇总，analysis 用 usage_stats.totals（同构结构）
function renderMonStats(t) {
  t = t || {};
  $('msReq').textContent = t.req != null ? t.req : '-';
  const errN = t.err || 0;
  $('msReqSub').innerHTML = '成功 ' + (t.ok || 0) + ' · 失败 <span' + (errN ? ' class="bad"' : '') + '>' + errN + '</span>';
  const tin = t.in_tokens || 0, tout = t.out_tokens || 0;
  $('msTok').textContent = formatTokenCount(tin + tout);
  $('msTokSub').textContent = '输入 ' + formatTokenCount(tin) + ' · 输出 ' + formatTokenCount(tout);
  $('msTtfb').textContent = formatLatency(t.ttfb_avg_ms);
  $('msTotal').textContent = (t.total_avg_sec != null && t.total_avg_sec >= 0) ? t.total_avg_sec.toFixed(1) + 's' : '—';
}

// entriesTotals 前端汇总：chatlogs 未回传 stats 时用可见条目估算（req 取 total 全量口径）
function entriesTotals(entries, total) {
  let ok = 0, err = 0, ttfbSum = 0, ttfbN = 0, durSum = 0, durN = 0, tin = 0, tout = 0;
  for (const e of entries) {
    if (e.status >= 200 && e.status < 400) ok++; else err++;
    if (e.ttfb_ms >= 0) { ttfbSum += e.ttfb_ms; ttfbN++; }
    if (e.total_sec >= 0) { durSum += e.total_sec; durN++; }
    if (e.in_tokens >= 0) tin += e.in_tokens;
    if (e.tokens >= 0) tout += e.tokens;
  }
  return {
    req: total != null ? total : entries.length, ok: ok, err: err,
    in_tokens: tin, out_tokens: tout,
    ttfb_avg_ms: ttfbN ? ttfbSum / ttfbN : null,
    total_avg_sec: durN ? durSum / durN : null,
  };
}

// fillMonUidOptions 账号筛选下拉：全部账号 + 各账号昵称；uid 列表没变就跳过重建
function fillMonUidOptions() {
  const sel = $('monUid');
  const accts = (overviewData && overviewData.accounts) || [];
  const sig = accts.map(a => a.uid).join(',');
  if (sig === monUidSig) return;
  monUidSig = sig;
  const prev = sel.value;
  sel.innerHTML = '<option value="">全部账号</option>' + accts.map(a =>
    '<option value="' + esc(a.uid) + '">' + esc(a.nickname || a.uid.slice(0, 8) + '…') + '</option>').join('');
  if (prev && accts.some(a => a.uid === prev)) sel.value = prev;
}

// showMonPanels 按页签显隐三块面板：detail/err 共用明细表，analysis/runtime 各自独立
function showMonPanels() {
  $('monListPanel').hidden = !(monTab === 'detail' || monTab === 'err');
  $('monAnalysisPanel').hidden = monTab !== 'analysis';
  $('monRuntimePanel').hidden = monTab !== 'runtime';
  $('monListTitle').textContent = monTab === 'err' ? '错误请求' : '请求明细';
}

// loadMonitoring 按当前页签拉对应数据；quiet=静默刷新（失败不 toast）。
// 顺带保活 overview：账号昵称映射（acctName）与 runtime 资源区都依赖 overviewData，
// 监控视图停留期间它不刷新就会陈旧（沿用旧 loadUsage 的行为）。
async function loadMonitoring(quiet) {
  loadOverview(true).then(fillMonUidOptions);
  fillMonUidOptions();
  if (monTab === 'analysis') { stopMonTimer(); await loadMonAnalysis(quiet); }
  else if (monTab === 'runtime') await loadMonRuntime(quiet); // 内部负责 5s 轮询的启停
  else { stopMonTimer(); await loadMonList(quiet); }
}

// loadMonList 明细/错误页签：服务端按时间窗+筛选+err_only 过滤，按 limit/offset
// 取一页。offset 分页下新日志入栈会把整页内容整体后移，翻到第 2 页还跟着静默刷新
// 的话，用户正在看的那几条会自己跑掉——因此静默刷新只在第 1 页生效（筛选/翻页/手动
// 刷新都不受影响）。
async function loadMonList(quiet) {
  const isErr = monTab === 'err';
  if (quiet && monPage > 1) return;
  // 一致性锚点：翻页/改每页条数都会发起新请求，先发的旧响应可能后落地。
  // 不认锚点就会出现"表体是第 1 页、页码写着第 2 页"，而且因为非首页吞掉静默
  // 刷新，这个错位不会自愈。锚点不匹配即丢弃该响应。
  const page = monPage, size = monPageSize;
  const extra = { limit: size, offset: (page - 1) * size };
  if (isErr) extra.err_only = 1;
  try {
    const d = await api('chatlogs?' + monQS(extra));
    if (page !== monPage || size !== monPageSize) return; // 用户已翻页或改了每页条数
    const total = d.total || 0;
    // 日志过期或筛选变窄都会让页数收缩，当前页可能越界：退到最后一页重取。
    // 收敛条件是"新页码不再越界"，故最多回退一次，不会反复递归。
    const pages = Math.max(1, Math.ceil(total / size));
    if (page > pages) { monPage = pages; return loadMonList(quiet); }
    monTotal = total;
    monEntries = d.entries || [];
    for (const e of monEntries) {
      const m = normModel(e.model);
      if (m && m !== '-') monSeenModels.add(m);
    }
    renderMonStats(d.stats || entriesTotals(monEntries, total));
    renderMonList(isErr, total);
  } catch (e) {
    if (quiet) return;
    // 取数失败：页码回滚到表体真实内容所在的那一页，保持"页码 ↔ 表体"一致
    if (monPage !== monRenderedPage) {
      monPage = monRenderedPage;
      renderMonPager(Math.max(1, Math.ceil(monTotal / monPageSize)));
    }
    toast(e.message, 'err');
  }
}

// renderMonList 明细表：服务端已按 ts 降序返回（最新在前），前端只做防御性排序；
// err 页签整行浅红底。Token 两行（↑输出/↓输入）、响应性能三行小字（首字/速率/总耗时），
// 错误列 CSS 截断 + title 全文。页脚同步刷新统计文案与分页条。
function renderMonList(isErr, total) {
  const tb = $('monBody');
  const entries = monEntries.slice().sort((a, b) => new Date(b.ts) - new Date(a.ts));
  if (!entries.length) {
    tb.innerHTML = '<tr><td colspan="8"><div class="empty">' +
      (isErr ? '该时段没有错误请求' : '该时段暂无请求（发一次 /v1/chat/completions 即会出现）') + '</div></td></tr>';
  } else {
    tb.innerHTML = entries.map(e => {
      const ok = e.status >= 200 && e.status < 400;
      const errText = e.err ? String(e.err) : '';
      return '<tr' + (isErr ? ' class="row-err"' : '') + '>' +
        '<td class="num" title="' + esc(fmtFullTime(e.ts)) + '">' + fmtRowTime(e.ts) + '</td>' +
        '<td title="' + esc(normModel(e.model)) + '">' + esc(normModel(e.model)) + '</td>' +
        '<td title="' + esc(acctName(e.uid)) + '">' + esc(acctName(e.uid)) + '</td>' +
        '<td>' + (e.mode === 'stream' ? '<span class="tag accent">流式</span>' : '<span class="tag mute">非流式</span>') + '</td>' +
        '<td><span class="tag ' + (ok ? 'ok' : 'bad') + '">' + esc(e.status) + '</span></td>' +
        '<td class="num"><div class="tok2 out"><i>↑</i>' + (e.tokens >= 0 ? formatTokenCount(e.tokens) : '—') + '</div>' +
        '<div class="tok2 in"><i>↓</i>' + (e.in_tokens >= 0 ? formatTokenCount(e.in_tokens) : '—') + '</div></td>' +
        '<td class="num"><div class="perf">首字 ' + formatLatency(e.ttfb_ms) + '</div>' +
        '<div class="perf">速率 ' + formatRate(e.tokps) + '</div>' +
        '<div class="perf">总耗时 ' + (e.total_sec >= 0 ? Number(e.total_sec).toFixed(1) + 's' : '—') + '</div></td>' +
        '<td title="' + esc(errText) + '">' + (errText ? esc(errText) : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
        '</tr>';
    }).join('');
  }
  const n = total != null ? total : entries.length;
  const pages = Math.max(1, Math.ceil(n / monPageSize));
  monRenderedPage = monPage; // 表体内容与页码的对应关系：失败回滚以它为准
  // 非首页不跟随实时刷新（见 loadMonList），把这条规则显式写出来，避免用户以为
  // 页面卡住了。
  $('monNote').innerHTML = '共 <b>' + n + '</b> 条 · 内存与磁盘保留最近 24 小时' +
    (monPage > 1 ? ' · <span class="bad">非首页已暂停实时刷新（回首页恢复）</span>' : '');
  renderMonPager(pages);
}

// renderMonPager 刷新分页条：页码显示、边界按钮禁用态、页码输入框上限。
function renderMonPager(pages) {
  const inp = $('monPgInput');
  $('monPgInfo').textContent = '第 ' + monPage + ' / ' + pages + ' 页';
  inp.max = pages;
  if (document.activeElement !== inp) inp.value = monPage; // 输入中不回写，避免打断输入
  $('monPgFirst').disabled = $('monPgPrev').disabled = monPage <= 1;
  $('monPgNext').disabled = $('monPgLast').disabled = monPage >= pages;
}

// gotoMonPage 跳到指定页：越界收敛到 [1, pages]，同页不重复取数
// （越界输入要把输入框弹回当前页，所以同页也要走一次 renderMonPager）。
function gotoMonPage(p) {
  const pages = Math.max(1, Math.ceil(monTotal / monPageSize));
  const next = Math.min(Math.max(1, Math.floor(p)), pages);
  if (next === monPage) { renderMonPager(pages); return; }
  monPage = next;
  loadMonitoring();
}

// commitMonPageInput 提交页码输入框：空串/非数字一律回弹当前页——Number('') 是 0
// （有限数），直接放行会"静默跳到第 1 页"，与用户的输入意图无关。number 输入框遇到
// 非法字符会清空 value，这条分支正是它的落点。
function commitMonPageInput() {
  const raw = $('monPgInput').value.trim();
  const p = Number(raw);
  if (raw === '' || !isFinite(p)) { renderMonPager(Math.max(1, Math.ceil(monTotal / monPageSize))); return; }
  gotoMonPage(p);
}

// loadMonAnalysis 用量分析页签：usage_stats 聚合（概览卡 + 趋势图 + 模型/账号分布）
async function loadMonAnalysis(quiet) {
  try {
    const d = await api('usage_stats?' + monQS());
    const t = d.totals || {};
    renderMonStats(t);
    $('aReq').textContent = t.req != null ? t.req : '—';
    $('aTok').textContent = formatTokenCount((t.in_tokens || 0) + (t.out_tokens || 0));
    $('aErr').textContent = t.err || 0;
    $('aModels').textContent = (d.models || []).length;
    drawMonChart(d);
    const totalReq = t.req || 0;
    const models = (d.models || []).slice().sort((a, b) => (b.req || 0) - (a.req || 0)).slice(0, 8);
    renderDist($('monModels'), models.map(m => ({ name: normModel(m.model), req: m.req || 0 })), totalReq);
    const accounts = (d.accounts || []).slice().sort((a, b) => (b.req || 0) - (a.req || 0)).slice(0, 8);
    renderDist($('monAccounts'), accounts.map(a => ({ name: acctName(a.uid), req: a.req || 0 })), totalReq);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

// niceStep y 轴刻度步长：取 1/2/5×10^n 的整步长，刻度控制在 3~5 档
function niceStep(raw) {
  if (!(raw > 0)) return 1;
  const pow = Math.pow(10, Math.floor(Math.log10(raw)));
  const n = raw / pow;
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * pow;
}

// drawMonChart 输入/输出 tokens 分组柱状图：DPR 缩放保高清、稀疏桶按索引均分、
// 桶下标签按密度抽稀显示 HH:mm，空数据画居中提示
function drawMonChart(d) {
  const cv = $('monChart');
  const buckets = d.buckets || [];
  const css = getComputedStyle(document.documentElement);
  const cIn = css.getPropertyValue('--accent').trim() || '#5b7cfa';
  const cOut = css.getPropertyValue('--ok').trim() || '#3ddc97';
  const cGrid = css.getPropertyValue('--line').trim() || '#262c3a';
  const cTxt = css.getPropertyValue('--ink-3').trim() || '#6b7488';
  const W = cv.clientWidth || cv.parentElement.clientWidth || 600, H = 220;
  const dpr = window.devicePixelRatio || 1;
  cv.width = Math.round(W * dpr);
  cv.height = Math.round(H * dpr);
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  if (!buckets.length) {
    ctx.fillStyle = cTxt;
    ctx.font = '13px ' + (css.getPropertyValue('--sans') || 'sans-serif');
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText('该时段暂无请求', W / 2, H / 2);
    return;
  }
  const padL = 48, padR = 10, padT = 12, padB = 24;
  const plotW = W - padL - padR, plotH = H - padT - padB;
  let maxV = 0;
  for (const b of buckets) maxV = Math.max(maxV, b.in_tokens || 0, b.out_tokens || 0);
  const step = niceStep(Math.max(1, maxV) / 3);
  const top = Math.max(step, Math.ceil(maxV / step) * step);
  ctx.font = '10.5px ' + (css.getPropertyValue('--mono') || 'monospace');
  ctx.textAlign = 'right';
  ctx.textBaseline = 'middle';
  for (let v = 0; v <= top + 1e-9; v += step) {
    const y = padT + plotH - (v / top) * plotH;
    ctx.strokeStyle = cGrid;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(padL, y + 0.5); // 半像素偏移：1px 网格线在 DPR=1 下不发虚
    ctx.lineTo(W - padR, y + 0.5);
    ctx.stroke();
    ctx.fillStyle = cTxt;
    ctx.fillText(formatTokenCount(Math.round(v)), padL - 6, y);
  }
  const n = buckets.length, slot = plotW / n;
  const barW = Math.max(2, Math.min(slot * 0.3, 16));
  ctx.textAlign = 'center';
  ctx.textBaseline = 'top';
  const labelEvery = Math.ceil(n / 12); // 桶太多时抽稀标签，避免互相重叠
  buckets.forEach((b, i) => {
    const cx = padL + slot * i + slot / 2;
    const hIn = (b.in_tokens || 0) / top * plotH;
    const hOut = (b.out_tokens || 0) / top * plotH;
    ctx.fillStyle = cIn;
    ctx.fillRect(cx - barW - 1, padT + plotH - hIn, barW, hIn);
    ctx.fillStyle = cOut;
    ctx.fillRect(cx + 1, padT + plotH - hOut, barW, hOut);
    if (i % labelEvery === 0) {
      const t = typeof b.t === 'number' ? b.t * 1000 : Date.parse(b.t);
      const dt = new Date(t);
      if (!isNaN(dt)) ctx.fillText(pad2(dt.getHours()) + ':' + pad2(dt.getMinutes()), cx, padT + plotH + 6);
    }
  });
}

// renderDist 分布行：名称 + 请求数/占比 + 占比横条（宽度按头部值归一，最小 3% 保底可见）
function renderDist(el, rows, totalReq) {
  if (!rows.length) { el.innerHTML = '<div class="dist-empty">该时段暂无数据</div>'; return; }
  const max = Math.max(1, ...rows.map(r => r.req));
  el.innerHTML = rows.map(r =>
    '<div class="dist-row">' +
    '<div class="t"><span class="nm" title="' + esc(r.name) + '">' + esc(r.name) + '</span>' +
    '<span class="num">' + r.req + ' · ' + (totalReq ? (r.req / totalReq * 100).toFixed(1) : '0.0') + '%</span></div>' +
    '<div class="bar2"><i style="width:' + Math.max(3, Math.round(r.req / max * 100)) + '%"></i></div>' +
    '</div>').join('');
}

// renderRuntimeRes 资源区：取自 overview 快照（版本/Redis/账号数/粘性会话随 overview 刷新）
function renderRuntimeRes() {
  const d = overviewData || {};
  const cells = [
    ['运行时长', d.uptime_sec != null ? dur(d.uptime_sec) : '—'],
    ['账号', (d.healthy != null ? d.healthy : '—') + ' / ' + (d.total != null ? d.total : '—')],
    ['粘性会话', d.sticky_sessions != null ? d.sticky_sessions : '—'],
    ['Redis 模式', d.redis_mode === 'upstash' ? 'Upstash 镜像' : '本地内存'],
    ['版本号', d.version ? 'v' + d.version : '—'],
  ];
  $('rtRes').innerHTML = cells.map(c =>
    '<div class="res-cell"><div class="rk">' + esc(c[0]) + '</div><div class="rv">' + esc(c[1]) + '</div></div>').join('');
}

// renderMonWindows 长窗口速率：60s 实时口径在低流量下常为 0，这里给
// 近 10 分钟 / 1 小时 / 24 小时的请求数与折算 TPM（metrics.windows，随轮询刷新）
function renderMonWindows(wins) {
  const label = sec => sec >= 86400 ? '近 24 小时' : sec >= 3600 ? '近 1 小时' : '近 10 分钟';
  const rows = (wins || []).map(w =>
    '<div class="res-cell" title="窗口内 ' + (w.req || 0) + ' 个请求 · 折算每分钟 Token"><div class="rk">' + esc(label(w.sec || 0)) + '</div>' +
    '<div class="rv">' + (w.req != null ? w.req : '-') + ' <span style="font-size:12px;color:var(--ink-3)">req · ' +
    (w.tpm != null ? formatTokenCount(w.tpm) : '—') + ' tok/min</span></div></div>');
  $('rtWindows').innerHTML = rows.length ? rows.join('') :
    '<div class="res-cell"><div class="rk">—</div><div class="rv">—</div></div>';
}

// renderAccountUsage 账号用量表（累计口径，数据源 overview.token_usage，随池持久化）：
// 平均延迟 = 累计耗时 ÷ 有延迟记录的尝试数；平均速率 = 有产出请求的累计输出 tokens ÷ 其累计耗时。
// 均值刻意不用 request_count/completion_tokens 当分母：历史计数没有对应累计值，混用会稀释失真。
function renderAccountUsage() {
  const tb = $('rtUsageBody');
  const list = ((overviewData || {}).accounts || []).slice().sort((a, b) => {
    const ra = (a.token_usage || {}).request_count || 0, rb = (b.token_usage || {}).request_count || 0;
    if (rb !== ra) return rb - ra;
    return (Date.parse((b.token_usage || {}).last_used_at || '') || 0) - (Date.parse((a.token_usage || {}).last_used_at || '') || 0);
  });
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="6"><div class="empty">暂无账号</div></td></tr>';
    return;
  }
  tb.innerHTML = list.map(s => {
    const tu = s.token_usage || {};
    const avgLat = (tu.latency_count > 0 && tu.sum_latency_ms > 0)
      ? formatLatency(tu.sum_latency_ms / tu.latency_count) : '—';
    const avgRate = (tu.active_latency_ms > 0 && tu.active_completion_tokens > 0)
      ? formatRate(tu.active_completion_tokens * 1000 / tu.active_latency_ms) : '—';
    return '<tr>' +
      '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + '</div><div class="id">' + esc(s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid) + '</div></td>' +
      '<td class="num">' + (tu.request_count || 0) + '</td>' +
      '<td class="num">' + formatTokenCount(tu.total_tokens) + '</td>' +
      '<td class="num" title="平均延迟 = 累计耗时 ÷ 有延迟记录的尝试数（含失败）">' + avgLat + '</td>' +
      '<td class="num" title="平均速率 = 有产出请求的累计输出 tokens ÷ 其累计耗时">' + avgRate + '</td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(tu.last_used_at) + '</td>' +
      '</tr>';
  }).join('');
}

// loadMonRuntime 运行状态页签：metrics 负载指标 + 5s 轮询（离开页签/视图即收）
async function loadMonRuntime(quiet) {
  renderRuntimeRes();
  renderAccountUsage();
  // 统计卡在 runtime 页签没有明细集合可依：按当前时间范围全量刷新（不带筛选），
  // 否则它会停留在上一个页签的筛选口径上，与"运行状态=全局视角"的语义不符。
  api('usage_stats?' + new URLSearchParams({ from: monWindow().from, to: monWindow().to }).toString())
    .then(d => renderMonStats(d.totals)).catch(() => {});
  try {
    const d = await api('metrics');
    $('rtInflight').textContent = d.in_flight != null ? d.in_flight : '-';
    $('rtRpm').textContent = d.rpm != null ? Math.round(d.rpm) : '-';
    $('rtTpm').textContent = d.tpm != null ? formatTokenCount(d.tpm) : '-';
    $('rtQps').textContent = d.qps != null ? Number(d.qps).toFixed(2) : '-';
    $('rtTps').textContent = d.tps != null ? Number(d.tps).toFixed(1) : '-';
    renderMonWindows(d.windows);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
  if (!monTimer) monTimer = setInterval(() => {
    if (view === 'monitoring' && monTab === 'runtime') {
      loadOverview(true); // 资源区（运行时长/账号/粘性会话）随轮询同步走
      loadMonRuntime(true);
    } else stopMonTimer(); // 页签/视图已切走：自行收掉，避免悬挂轮询
  }, 5000);
}

function stopMonTimer() { if (monTimer) { clearInterval(monTimer); monTimer = null; } }

// scheduleMonReload 模型输入防抖：停输 300ms 才重拉，避免逐键打接口。
// 筛选变化会换掉整个结果集，原页码随即失去意义，故一并回第 1 页。
function scheduleMonReload() {
  monPage = 1;
  if (monReloadTimer) clearTimeout(monReloadTimer);
  monReloadTimer = setTimeout(() => { monReloadTimer = null; loadMonitoring(); }, 300);
}

/* ── 监控：模型筛选目录（可搜索下拉） ─────────────────────────────── */
/* 数据=后端 models_all（并发拉全部账号的目录按 realm 并集去重）；目录懒加载
   （首次展开才拉），失败退回明细里观测到的模型名。输入即含匹配过滤（剥前缀
   双向包含），点击条目回填输入框并防抖重拉明细。 */
let modelCatalog = null; // null=未加载；数组=已加载（含空数组=加载过但为空）

// observedModels 兜底模型名：取跨页累积的观测集合（loadMonList 每次落地都往里塞），
// 而不是当前页——分页后只看当前页会让下拉目录随翻页缩水。
function observedModels() {
  return Array.from(monSeenModels).sort();
}

function renderMonModelDd() {
  const dd = $('monModelDd');
  if (dd.hidden || !Array.isArray(modelCatalog)) return;
  const q = bareModel($('monModel').value.trim()).toLowerCase();
  const items = (q ? modelCatalog.filter(m => bareModel(m).toLowerCase().includes(q)) : modelCatalog.slice())
    .slice(0, 80); // 防超长目录把下拉撑到离谱；继续输入可进一步收窄
  monDdQuery = q;
  if (!items.length && !modelCatalog.length) {
    dd.innerHTML = '<div class="mdd-note">模型目录为空</div>';
    return;
  }
  dd.innerHTML = items.length
    ? items.map(m => '<button type="button" class="mdd-item" data-v="' + esc(m) + '" title="' + esc(m) + '">' + esc(m) + '</button>').join('')
    : '<div class="mdd-note">没有包含「' + esc($('monModel').value.trim()) + '」的模型</div>';
}

function placeMonModelDd() {
  const inp = $('monModel'), dd = $('monModelDd');
  const r = inp.getBoundingClientRect();
  dd.style.left = Math.max(8, Math.min(r.left, innerWidth - 320)) + 'px';
  dd.style.top = (r.bottom + 5) + 'px';
  dd.style.minWidth = Math.max(r.width, 240) + 'px';
}

async function openMonModelDd() {
  const dd = $('monModelDd');
  dd.hidden = false;
  placeMonModelDd();
  if (!modelCatalog) {
    dd.innerHTML = '<div class="mdd-note"><span class="dots">正在获取所有账号的模型目录</span></div>';
    try {
      const d = await api('models_all');
      modelCatalog = d.models || [];
    } catch (e) {
      // 目录拿不到就用明细观测值兜底；观测值也为空则保持未加载，下次展开重试
      const obs = observedModels();
      if (!obs.length) {
        dd.innerHTML = '<div class="mdd-note">获取失败：' + esc(e.message) + '（重开下拉重试）</div>';
        return;
      }
      modelCatalog = obs;
    }
  }
  if (dd.hidden) return; // 等待期间用户已关闭
  renderMonModelDd();
}

function closeMonModelDd() { $('monModelDd').hidden = true; }

$('monModelCaret').onclick = () => {
  if ($('monModelDd').hidden) { openMonModelDd(); $('monModel').focus(); }
  else closeMonModelDd();
};
$('monModel').addEventListener('focus', openMonModelDd);
$('monModel').addEventListener('input', () => {
  monFilters.model = $('monModel').value.trim();
  if ($('monModelDd').hidden) openMonModelDd();
  else if (bareModel($('monModel').value.trim()).toLowerCase() !== monDdQuery) renderMonModelDd();
  scheduleMonReload();
});
$('monModel').addEventListener('keydown', ev => {
  if (ev.key === 'Escape') { closeMonModelDd(); ev.stopPropagation(); }
});
$('monModelDd').addEventListener('click', ev => {
  const b = ev.target.closest('.mdd-item');
  if (!b) return;
  $('monModel').value = b.dataset.v;
  monFilters.model = b.dataset.v;
  closeMonModelDd();
  $('monModel').focus();
  scheduleMonReload();
});
// 点击下拉与筛选框之外收起（bubble 阶段：▾ 按钮自己的 toggle 先执行，不受影响）
document.addEventListener('click', ev => {
  if ($('monModelDd').hidden) return;
  if (!ev.target.closest('#monModelDd') && !ev.target.closest('#monModelWrap')) closeMonModelDd();
});
// 窗口缩放 / 任意容器滚动时跟随输入框位置（capture 捕获 .chat-wrap 内部滚动）
addEventListener('resize', () => { if (!$('monModelDd').hidden) placeMonModelDd(); });
addEventListener('scroll', () => { if (!$('monModelDd').hidden) placeMonModelDd(); }, true);

/* ── 监控：控件事件 ───────────────────────────────────────────────── */
$('monTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-tab]');
  if (!b) return;
  monTab = b.dataset.tab;
  monPage = 1; // 换页签 = 换结果集，页码回到第 1 页
  document.querySelectorAll('#monTabs .chip').forEach(c => c.classList.toggle('on', c === b));
  showMonPanels();
  loadMonitoring();
});
/* 时间窗/账号/模式筛选：任一变化都会换掉结果集，页码一律回第 1 页（模型输入框走
   scheduleMonReload，那里同样回第 1 页）。翻页本身只改 monPage，不动筛选。 */
$('monRange').onchange = () => { monRange = $('monRange').value; monPage = 1; loadMonitoring(); };
$('monUid').onchange = () => { monFilters.uid = $('monUid').value; monPage = 1; loadMonitoring(); };
$('monMode').onchange = () => { monFilters.mode = $('monMode').value; monPage = 1; loadMonitoring(); };
$('monRefresh').onclick = () => loadMonitoring(); // 手动刷新保留当前页
// 重置：清空筛选 + 时间范围回默认，并立即重拉
$('monReset').onclick = () => {
  monRange = '24h';
  $('monRange').value = '24h';
  monFilters = { uid: '', model: '', mode: '' };
  $('monUid').value = ''; $('monModel').value = ''; $('monMode').value = '';
  monPage = 1;
  loadMonitoring();
};

/* 分页控件：首页/上一页/下一页/末页 + 页码直跳 + 每页条数。
   翻页只重取目标页；每页条数变化会让原页码失去意义，回第 1 页。
   页码输入框：change（数字框上下箭头/失焦）+ Enter 都提交，非法值由
   commitMonPageInput 弹回，越界值由 gotoMonPage 收敛。 */
$('monPgFirst').onclick = () => gotoMonPage(1);
$('monPgPrev').onclick = () => gotoMonPage(monPage - 1);
$('monPgNext').onclick = () => gotoMonPage(monPage + 1);
$('monPgLast').onclick = () => gotoMonPage(Math.ceil(monTotal / monPageSize));
$('monPgInput').addEventListener('change', commitMonPageInput);
$('monPgInput').addEventListener('keydown', ev => {
  if (ev.key === 'Enter') { ev.preventDefault(); commitMonPageInput(); }
});
$('monPgSize').onchange = () => {
  monPageSize = Number($('monPgSize').value) || 100;
  monPage = 1;
  loadMonitoring();
};

/* ── 监控：CSV 导出 ───────────────────────────────────────────────── */
/* 按当前筛选分页拉满（单页 2000、总量上限 10000 条防失控），带 BOM 的 CSV 经
   Blob 下载；值含逗号/引号/换行时包双引号并转义内部引号。 */
function csvStamp() {
  const d = new Date();
  return '' + d.getFullYear() + pad2(d.getMonth() + 1) + pad2(d.getDate()) + '-' + pad2(d.getHours()) + pad2(d.getMinutes()) + pad2(d.getSeconds());
}
function csvTime(ts) {
  const d = new Date(ts);
  if (isNaN(d)) return '';
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' +
    pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds());
}
function csvCell(v) {
  const s = v == null ? '' : String(v);
  return /[",\n\r]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}
$('monExport').onclick = async () => {
  const b = $('monExport');
  b.disabled = true;
  b.textContent = '导出中…';
  try {
    const LIMIT = 2000, CAP = 10000;
    const extra = { limit: LIMIT, offset: 0 };
    if (monTab === 'err') extra.err_only = 1;
    const rows = [];
    let total = Infinity;
    for (let offset = 0; offset < CAP && rows.length < total; offset += LIMIT) {
      extra.offset = offset;
      const d = await api('chatlogs?' + monQS(extra));
      const entries = d.entries || [];
      rows.push(...entries);
      if (d.total != null) total = d.total;
      if (entries.length < LIMIT) break;
    }
    if (!rows.length) { toast('当前筛选下没有可导出的请求'); return; }
    const head = ['时间', '模型', '账号', '类型', '状态', '输入tokens', '输出tokens', '首字ms', '速率tok/s', '总耗时s', '错误'];
    const lines = [head.join(',')];
    for (const e of rows) {
      lines.push([
        csvTime(e.ts),
        normModel(e.model),
        acctName(e.uid),
        e.mode === 'stream' ? '流式' : '非流式',
        e.status,
        e.in_tokens >= 0 ? e.in_tokens : '',
        e.tokens >= 0 ? e.tokens : '',
        e.ttfb_ms >= 0 ? Math.round(e.ttfb_ms) : '',
        e.tokps >= 0 ? Number(e.tokps).toFixed(1) : '',
        e.total_sec >= 0 ? Number(e.total_sec).toFixed(1) : '',
        e.err || '',
      ].map(csvCell).join(','));
    }
    const url = URL.createObjectURL(new Blob(['\uFEFF' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = 'requests-' + csvStamp() + '.csv';
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 4000);
    toast('已导出 ' + rows.length + ' 条请求记录', 'ok');
  } catch (e) { toast('导出失败：' + e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '导出 CSV'; }
};

/* ── 日志（频道：全部/任务/系统。对话日志在「监控」视图，此处不再展示） ── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  loadLogs();
});
async function loadLogs() {
  const box = $('logBox');
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  try {
    const d = await api('logs');
    const entries = (d.entries || []).filter(e => e.ch !== 'chat' && (logCh === 'all' || e.ch === logCh));
    box.innerHTML = entries.length
      ? entries.map(e => {
        const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
        const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
        const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || e.ch) + '</i>' : '';
        return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
      }).join('')
      : '<span style="color:var(--ink-3)">暂无日志</span>';
    if (logPin && atEnd) box.scrollTop = box.scrollHeight;
    const counts = {};
    for (const e of (d.entries || [])) { if (e.ch !== 'chat') counts[e.ch] = (counts[e.ch] || 0) + 1; }
    $('logNote').textContent = logCh === 'all'
      ? '任务 ' + (counts.task || 0) + ' · 系统 ' + (counts.sys || 0)
      : (logCh === 'task' ? '任务' : '系统') + ' ' + entries.length + ' 行';
  } catch (e) { /* 概览已提示 */ }
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};

/* ── 配置 ─────────────────────────────────────────────────────────── */
/* listen 不在此表中：面板页把它呈现为只读的 API 地址（按当前访问地址推导），
   不再作为可编辑字段提交——保存时 merge 会保留磁盘上的原值。 */
const CFG_MAP = {
  api_key: ['api_key'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  max_body_mb: ['server', 'max_body_mb'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
};
// apiBaseURL 面板实际生效的 OpenAI 兼容基址（base_url）。
// 用 location.origin 而非配置里的 listen：listen 的 host 部分可能是 ":7863"
// （= 绑全部网卡）或 "0.0.0.0:7863"，两者都不是客户端能直接填的地址；用户访问
// 面板用的地址才是可达地址。桌面端由 forceLoopback 固定为 127.0.0.1。
function apiBaseURL() { return location.origin + '/v1'; }
function renderApiAddr() {
  const el = $('cfgApiAddr');
  if (!el) return;
  el.value = apiBaseURL();
}
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  renderApiAddr(); // 本地推导（location.origin），不依赖接口；接口失败也要显示
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? '' : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      if (raw === '') v = undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCopyAddr').onclick = () => copyField($('cfgApiAddr'), 'API 地址已复制');
$('btnCopyKey').onclick = () => copyField($('cfgKey'), 'API 密钥已复制');
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到选域态：选域可见、加载/就绪/完成/错误全收，起始按钮亮起。
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
  const b = $('btnRefresh');
  b.disabled = true; b.textContent = '刷新中…';
  try {
    await api('balance_all', { method: 'POST' });
    await loadOverview(true);
    toast('余额已从上游刷新', 'ok');
  } catch (e) { toast('刷新失败：' + e.message, 'err'); await loadOverview(true); }
  finally { b.disabled = false; b.textContent = '刷新'; }
  if (view === 'logs') loadLogs();
};

/* ── 实时刷新 ─────────────────────────────────────────────────────── */
/* 面板不做网络轮询。池状态在服务端发生变更的那一刻，经 SSE（GET /panel/api/events）
   推一个信号过来，收到就刷一次——「模型开始跑」到「面板点亮」之间只剩一次本地
   overview 往返（实测 2~3ms），不必再等下一个采样点。
   刷新次数只跟池的真实状态变更次数挂钩，与请求耗时无关：流式响应不论推多少 chunk，
   池侧只在开始/计量/成功/释放这几个点变更，因此不会越刷越多。
   EventSource 不能带 Authorization 头，所以用 fetch 读 text/event-stream 自己拆帧，
   鉴权与其它接口一致走 Bearer。 */
const EVENTS_PATH = '/panel/api/events';
const EVENTS_RETRY_MS = 3000;    // 已建过连的推送断线后的重连间隔
const EVENTS_STALE_MS = 45000;   // 这么久收不到任何帧（含服务端 25s 心跳）判定为假死
const EVENTS_WATCH_MS = 10000;   // 假死检测的检查周期
const EVENTS_COALESCE_MS = 120;  // 事件合并窗口（首个事件不等待，见 onPoolEvent）
const POLL_FALLBACK_MS = 3000;   // 仅当推送建不起来时的兜底轮询间隔

let evAbort = null, evRetryTimer = null, evWatchTimer = null;
let evLastFrame = 0, evEverOk = false, evDegraded = false, evPending = null, evLastFire = 0;

function refreshVisible() {
  if (view === 'accounts') loadOverview(true);
  else if (view === 'monitoring') loadMonitoring(true);
  else if (view === 'logs') loadLogs();
  else if (view === 'taskscenter') pollQueueOnce();
}

// onPoolEvent = 前沿立即刷 + 尾随防抖。一次请求会依次触发
// Acquire / RecordTokenUsage / NoteSuccess / Release 多次通知，其中最关键的一拍是
// 请求开始（点亮）。合并只把"挨得近"的通知压成一拍，不跨窗口——相隔超过一个窗口的
// 真实状态变更各刷各的，不会互相吞掉。
//   前沿立即刷：首个事件不等窗口，这就是"账号被调用即点亮"的那一次，延迟只剩
//     一次本地 overview 往返（实测个位数毫秒）。纯尾随合并会白白拉长一个窗口。
//   尾随防抖：窗口内到达的事件不断把刷新往后推，直到事件静默才补刷一次。
//     若改成固定节流，请求尾部那串跨度超过一个窗口的通知会分裂成多次刷新——
//     数据没变时虽被渲染指纹拦在 DOM 之外，但请求照发。
function onPoolEvent() {
  const now = Date.now();
  if (now - evLastFire >= EVENTS_COALESCE_MS) {
    evLastFire = now;
    refreshVisible();
    return;
  }
  if (evPending) clearTimeout(evPending);
  evPending = setTimeout(() => {
    evPending = null;
    evLastFire = Date.now();
    refreshVisible();
  }, EVENTS_COALESCE_MS);
}

// 假死看门狗：对端消失但 socket 未关闭时 read() 会一直挂着，不报错也不 EOF。
// 靠"多久没收到帧"判断，主动 abort 逼出 catch 再立即重连。
function armEventWatch() {
  if (evWatchTimer) clearInterval(evWatchTimer);
  evWatchTimer = setInterval(() => {
    if (Date.now() - evLastFrame <= EVENTS_STALE_MS) return;
    const old = evAbort;
    evAbort = null;
    if (old) old.abort();
    connectEvents();
  }, EVENTS_WATCH_MS);
}

async function connectEvents() {
  if (evAbort || evDegraded) return;
  const ctl = new AbortController();
  evAbort = ctl;
  const key = localStorage.getItem(LS_KEY);
  let gotStream = false;
  try {
    const r = await fetch(EVENTS_PATH, {
      headers: key ? { Authorization: 'Bearer ' + key } : {},
      signal: ctl.signal, cache: 'no-store',
    });
    if (!r.ok || !r.body) throw new Error('HTTP ' + r.status);
    gotStream = true;
    evEverOk = true;
    evLastFrame = Date.now();
    armEventWatch();
    const rd = r.body.getReader(), dec = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await rd.read();
      if (done) break;
      evLastFrame = Date.now();
      buf = (buf + dec.decode(value, { stream: true })).replace(/\r\n/g, '\n');
      let i;
      // SSE 帧以空行分隔。本面板不关心帧内容，只把"有帧"当作"池状态可能变了"。
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, i);
        buf = buf.slice(i + 2);
        if (frame.trim() === '' || /^\s*:/.test(frame)) continue; // 空行 / 注释心跳
        onPoolEvent();
      }
    }
    throw new Error('服务端关闭了推送连接');
  } catch (e) {
    // 主动断开（假死重连、重新进入面板）时由发起方接管重连，这里不再插手
    if (ctl.signal.aborted) return;
    if (!gotStream && !evEverOk) { degradeToPoll(e); return; }
    if (evRetryTimer) clearTimeout(evRetryTimer);
    evRetryTimer = setTimeout(() => { evRetryTimer = null; connectEvents(); }, EVENTS_RETRY_MS);
  } finally {
    if (evAbort === ctl) evAbort = null;
  }
}

// degradeToPoll：推送在当前环境不可用（二进制没这个端点、中间层剥掉流式响应、
// 鉴权不通过等）。按"降级要出声"的约定明确告知，而不是静默留一块不再刷新的面板。
function degradeToPoll(e) {
  if (evDegraded) return;
  evDegraded = true;
  toast('实时推送不可用（' + e.message + '），已降级为 ' + (POLL_FALLBACK_MS / 1000) + ' 秒轮询', 'err');
  if (refTimer) clearInterval(refTimer);
  refTimer = setInterval(refreshVisible, POLL_FALLBACK_MS);
}

// 本地时钟：不发任何请求，只把随时间自变的显示（冷却倒计时、"刚被调用过"）推进。
// 「哪个账号在用」由服务端事件驱动，不依赖这一拍。
function tickLocal() {
  if (view === 'accounts') {
    if (overviewData) renderAccounts(overviewData.accounts || []);
  } else if (view === 'taskscenter') pollQueueOnce();
}

function start() {
  loadOverview(true);
  if (localTimer) clearInterval(localTimer);
  localTimer = setInterval(tickLocal, 1000);
  // 重新进入（首次 / 换密钥）时重置降级态：上一次失败可能是密钥不对造成的，
  // 换了密钥值得再试一次推送。
  if (evDegraded) {
    evDegraded = false;
    evEverOk = false;
    if (refTimer) { clearInterval(refTimer); refTimer = null; }
  }
  connectEvents();
  checkAuthGate();
}
async function checkAuthGate() {
  try { await api('overview'); }
  catch (e) { if (String(e.message).includes('密钥') || String(e.message).includes('api_key')) return; }
}
start();

/* ── 积分任务 ─────────────────────────────────────────────────────── */
let taskUID = null;

// 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
// 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
// 注意：键含点号（Model_chat_GLM5.2）必须加引号，否则会被解析成属性访问 + 数字字面量。
const AUTO_TASKS = {
  'chat_5': '上报 5 条对话活跃事件（自动补足差额）',
  'first_buddy': '上报解锁 → 同意协议 → 领取第一只 Buddy',
  'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
  'RichMeow_Chat': '桌面指纹事件链上报（已验证可点亮）',
  'Buddy_App': '上报「进入 Buddy 应用」事件链（已验证可点亮）',
  'Buddy_App_QQ': '上报「进入企鹅教师助手」事件链（已验证可点亮）',
  'automation_1': '上报「定时任务创建」事件（已验证可点亮）',
  'Library_read': '上报「读资料库介绍」事件（已验证可点亮）',
  'template_5': '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
  'playbook_prompt': '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
  'create_canvas': '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
  'expert_5': '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
  'Expert_team_use_3': '真实专家团召唤+使用链 ×3（三账号实测点亮）',
  'Hp_Appearance': '设置主题 API + 皮肤生效事件（两账号实测点亮）',
  'black_cat': '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
  'Expert_lighthouse': '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
  'skill_1': '真实对话 + skill_info 技能加载事件（实测点亮）'
};

function openTasks(uid) {
  taskUID = uid;
  $('taskWho').textContent = uid.slice(0, 16);
  $('taskVeil').classList.add('on');
  $('btnTaskReload').hidden = false;
  loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// 全部接受：把该账号未接受的任务一次性报名（幂等，跳过已接受/已领取）。
$('btnTaskAcceptAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAcceptAll');
  btn.disabled = true; btn.textContent = '接受中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
    const n = r.accepted || 0;
    if (r.failed && r.failed.length) {
      toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
    } else {
      toast(n ? `已接受 ${n} 个任务` : (r.message || '所有任务均已接受'), 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部接受'; loadTasks(); }
};

// 一键完成全部可自动任务（耗时较长：含真实对话，逐项回读验证）。
$('btnTaskAutoAll').onclick = async () => {
  if (!taskUID) return;
  const btn = $('btnTaskAutoAll');
  if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
  btn.disabled = true; btn.textContent = '执行中…';
  try {
    const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
    const okN = (r.results || []).filter(x => x.status === 'done').length;
    const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
    const errN = (r.results || []).filter(x => x.status === 'error').length;
    toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    console.log('auto_all results:', r.results);
  } catch (e) { toast(e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '一键完成可自动任务'; loadTasks(); }
};

async function loadTasks() {
  if (!taskUID) return;
  const st = $('taskState'), tb = $('taskTable');
  st.hidden = false;
  st.className = 'state';
  st.innerHTML = '<span class="dots">查询中</span>';
  tb.hidden = true;
  try {
    const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
    const list = d.tasks || [];
    if (!list.length) {
      st.className = 'state';
      st.textContent = '该账号暂无任务';
      return;
    }
    // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
    list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
    $('taskBody').innerHTML = list.map(t => {
      // 进度：current 可能缺失（0 或被上游省略）——用 ?? 兜底，避免渲染成 "undefined / N"
      const cur = t.current ?? 0, tgt = t.target ?? 0;
      const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
      const parts = [];
      if (t.credit) parts.push('+' + t.credit + ' 分');
      if (t.energy) parts.push('+' + t.energy + ' 能');
      if (t.reward_buddy) parts.push('Buddy');
      const reward = parts.length ? parts.join(' ') : '—';
      const badge = t.claimed ? '<span class="tag ok">已领取</span>'
        : t.claimable ? '<span class="tag warn">可领取</span>'
        : t.locked ? '<span class="tag mute">未解锁</span>'
        : t.accept_status === 'accepted' ? '<span class="tag mute">进行中</span>'
        : '<span class="tag mute">未接受</span>';
      const acted = t.claimed || t.locked ? ''
        : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">领取</button>'
        : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">一键完成</button>'
        : t.accept_status === 'accepted' ? ''
        : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">接受</button>';
      // 操作指引（description/task_desc）挂 title 提示：如何完成交给用户看
      const tip = [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
      return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
        '<td class="num">' + esc(prog) + '</td>' +
        '<td class="num">' + esc(reward) + '</td>' +
        '<td>' + badge + '</td>' +
        '<td class="c-acts"><div class="acts">' + acted + '</div></td></tr>';
    }).join('');
    st.hidden = true;
    tb.hidden = false;
  } catch (e) {
    st.className = 'state err';
    st.textContent = e.message;
  }
}

$('taskBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-t]');
  if (!b || !taskUID) return;
  const kind = b.dataset.t, code = b.dataset.c;
  b.disabled = true;
  try {
    if (kind === 'auto') {
      // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
      b.textContent = '执行中…';
      const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
        method: 'POST', body: JSON.stringify({ task_code: code })
      });
      if (r.skipped) {
        toast(r.message || '已跳过', 'ok');
      } else {
        const advanced = r.progress_before !== r.progress_after;
        let msg = r.message || '已执行';
        if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`;
        if (r.claimed) msg += '，奖励已自动到账';
        else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : '';
        else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端';
        toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
      }
      loadOverview(true);
    } else {
      const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
      const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
      await api(path, { method: 'POST', body: JSON.stringify(body) });
      toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
      if (kind === 'claim') loadOverview(true);
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { loadTasks(); }
});

/* ── 任务中心：开学季 + 全账号扫描/队列 ──────────────────────────── */
const SCHOOL_META = [
  ['share_invite', '分享'],
  ['desktop_chat_1_time', '桌面'],
  ['chat_3_times', '对话×3'],
  ['expert_use', '专家'],
  ['task_student_verify', '认证'],
];
// 开学季任务单元：✓ 已领（绿）｜◐ x/y 进行中（琥珀）｜○ 未做（灰）
function staskHTML(t) {
  if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
  if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>已领</span>';
  if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>可领</span>';
  if (t.status === 'in_progress') {
    const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
    return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
  }
  return '<span class="stask todo"><span class="mark">○</span>未做</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
async function loadSchoolStatus(quiet) {
  const st = $('schoolState'), list = $('schoolList');
  if (!quiet) { st.hidden = false; st.className = 'state'; st.innerHTML = '<span class="dots">查询中</span>'; list.innerHTML = ''; }
  try {
    const d = await api('school/status');
    const arr = d.accounts || [];
    if (!arr.length) {
      st.hidden = false; st.className = 'state'; st.textContent = '暂无可用账号';
      list.innerHTML = ''; return;
    }
    let allDone = 0;
    const head = '<div class="shead"><div class="who">账号</div><div class="stasks">' +
      SCHOOL_META.map(([, name]) => '<span>' + esc(name) + '</span>').join('') +
      '</div><div class="luck">剩余抽奖</div></div>';
    list.innerHTML = head + arr.map(v => {
      const by = {};
      (v.tasks || []).forEach(t => by[t.task_code] = t);
      const cells = SCHOOL_META.map(([code]) => {
        const t = by[code];
        const html = code === 'task_student_verify'
          ? '<span class="stask todo"><span class="mark">—</span>不做</span>'
          : staskHTML(t);
        return '<span title="' + esc(SCHOOL_TITLES[code] || code) + '">' + html + '</span>';
      }).join('');
      const done = SCHOOL_META.filter(([code]) => code !== 'task_student_verify' && by[code] && by[code].status === 'claimed').length;
      allDone += done === 4 ? 1 : 0;
      return '<div class="srow">' +
        '<div class="who"><div class="nm" title="' + esc(v.nickname || '') + '">' + esc(v.nickname || '未命名') + '</div><div class="id">' + esc(v.uid) + '</div></div>' +
        '<div class="stasks">' + cells + '</div>' +
        '<div class="luck" title="剩余抽奖次数">' + LUCK_SVG + (v.chances == null ? '—' : v.chances) + '</div>' +
        (v.error ? '<div class="err">' + esc(v.error) + '</div>' : '') +
        '</div>';
    }).join('');
    $('schoolSummary').textContent = allDone === arr.length ? '今日全部完成 🎉' : allDone + '/' + arr.length + ' 个账号今日全部完成';
    st.hidden = true;
  } catch (e) {
    st.hidden = false; st.className = 'state err'; st.textContent = e.message;
  }
}
const SCHOOL_TITLES = {
  share_invite: '分享活动 +100c', desktop_chat_1_time: '桌面端体验 +100c（单次）',
  chat_3_times: '和 AI 对话 3 次 +50c', expert_use: '召唤开学季专家 +50c',
  task_student_verify: '学生认证 +100c（需真实认证，不做）',
};
$('btnSchoolRefresh').onclick = () => loadSchoolStatus(false);
$('btnSchoolRunAll').onclick = async () => {
  if (!confirm('将对全部账号执行开学季闭环（分享/桌面/对话/专家 + 抽奖），约 1-2 分钟。确认继续？')) return;
  try {
    await api('school/run_all', { method: 'POST' });
    toast('开学季闭环已开始，结果看任务日志', 'ok');
    setTimeout(() => loadSchoolStatus(true), 15000);
  } catch (e) { toast(e.message, 'err'); }
};

/* ── 精简 QR 编码器（券码二维码用）────────────────────────────────────
   规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
   完整性：规范允许任选掩码（解码器按格式信息位自行去掩码），固定掩码不影响
   可扫描性；已用 python qrcode 库对多输入多版本做逐像素交叉验证（强制 byte
   模式 + mask 0，5/5 全部 diff=0）。面板 CSP 只允许 self，外链 QR 服务不可用。 */
// qr_gen.js —— 精简 QR 编码器（浏览器用 + node 可跑交叉验证）
// 规格子集：byte 模式、ECC L、版本 1-5（全部单纠错块，免块交织）、固定掩码 0。
// 完整性说明：规范允许编码器任选掩码（解码器按格式信息位自行去掩码），
// 固定掩码不影响可扫描性；券码为短文本，v1-5（26 字节起）绰绰有余。

// GF(256) 对数/指数表（本原多项式 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
  for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// 各版本参数（下标 = 版本-1）：[数据码字数, 纠错码字数]，ECC L 单块
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// 对齐图案中心坐标（v2+；与定位图案重叠的位置在放置时跳过）
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // 掩码模式 0

// 生成多项式（最高次系数在前，g[0] 恒为 1）
function qrGenPoly(deg) {
  let g = [1];
  for (let i = 0; i < deg; i++) {
    const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
    ng[0] = g[0];
    for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
    ng[g.length] = gmul(a, g[g.length - 1]);
    g = ng;
  }
  return g;
}

// Reed-Solomon 求余（综合除法），返回 deg 个纠错码字
function rsRem(data, deg) {
  const g = qrGenPoly(deg);
  const res = data.concat(new Array(deg).fill(0));
  for (let i = 0; i < data.length; i++) {
    const f = res[i];
    if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
  }
  return res.slice(data.length);
}

// 文本 → 码字流（byte 模式：0100 + 8 位计数 + 数据 + 终止符 + 0xEC/0x11 填充）
function qrDataCodewords(text, dataCap) {
  const bytes = Array.from(new TextEncoder().encode(text));
  const bits = [];
  const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
  push(4, 4);            // byte 模式
  push(bytes.length, 8); // v1-9 计数 8 位
  for (const b of bytes) push(b, 8);
  const cap = dataCap * 8;
  push(0, Math.min(4, cap - bits.length));   // 终止符
  while (bits.length % 8) bits.push(0);
  const out = [];
  for (let i = 0; i < bits.length; i += 8) {
    let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
    out.push(v);
  }
  for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
  return out;
}

// 主入口：text → 布尔矩阵（true=深色模块）
function qrMatrix(text) {
  const bytes = Array.from(new TextEncoder().encode(text));
  // 版本选择：需求 ≈ 2 码字头 + 文本长度，取首个放得下的版本
  let ver = 0;
  for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
  if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
  const [dataCap, ecCap] = QR_V[ver - 1];
  const n = 17 + 4 * ver;

  const M = Array.from({ length: n }, () => new Array(n).fill(false));
  const F = Array.from({ length: n }, () => new Array(n).fill(false)); // 功能模块占位

  const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
  // 定位图案 + 分隔带
  const finder = (r0, c0) => {
    for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
      const rr = r0 + r, cc = c0 + c;
      if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
      const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
      setF(rr, cc, dark);
    }
  };
  finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
  // 校正图形（仅贯穿两定位图案之间：8..n-9，不得覆盖定位图案本体）
  for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
  for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
  // 对齐图案（v2+，跳过与定位重叠处）
  const align = QR_ALIGN[ver - 1] || [];
  for (const ar of align) for (const ac of align) {
    if (F[ar][ac]) continue;
    for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
      setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
  }
  // 暗模块 + 格式信息（ECC L=01，掩码 0）——BCH(15,5) + 0x5412 异或。
  // 位序遵循规范（与 python qrcode 逐位对齐验证）：bit i 从 LSB 起数，
  // 副本一走左上角 L 形、副本二走右下 L 形。
  let fmt = (1 << 3) | 0; // L<<3 | mask
  let rem = fmt << 10;
  for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
  fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 位
  const fb = i => (fmt >> i) & 1;
  // 副本一（左上）：位 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
  for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
  setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
  // 副本一续 + 副本二（右下）：位 8..14 → (n-15+i, 8)；位 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
  for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
  for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
  setF(8, 7, !!fb(8));
  for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
  // 暗模块（恒为深色，位于副本一垂直段末端）
  setF(n - 8, 8, true);


  // 数据码字 + 纠错码字 → 位流
  const dcw = qrDataCodewords(text, dataCap);
  const cw = dcw.concat(rsRem(dcw, ecCap));
  const bits = [];
  for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

  // 蛇形放置（成对列，从右向左，跳过第 6 列），写数据时直接异或掩码
  let bi = 0, up = true;
  for (let x = n - 1; x > 0; x -= 2) {
    if (x === 6) x--;
    for (let i = 0; i < n; i++) {
      const r = up ? n - 1 - i : i;
      for (const c of [x, x - 1]) {
        if (F[r][c]) continue;
        const bit = bi < bits.length ? bits[bi++] : 0;
        M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
      }
    }
    up = !up;
  }
  return M;
}

// 矩阵 → SVG（quiet zone 4 模块）
function qrSVG(M, px) {
  const n = M.length, q = 4, total = n + q * 2;
  let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
  for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
    if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
  return s + '</svg>';
}

/* ── 开学季券码查询（弹窗，仿活动页 #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API 只在 secure context（https/localhost）可用，
   远程 http 面板会拿不到 navigator.clipboard → 降级 execCommand。 */
function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
    catch (e) { reject(e); }
    finally { ta.remove(); }
  });
}

function vcCard(v) {
  const expired = v.valid_to && new Date(v.valid_to) < new Date();
  return '<div class="vc' + (expired ? ' expired' : '') + '">' +
    '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || '券') + '</span>' +
    (expired ? '<span class="tag bad">已过期</span>' : '<span class="tag ok">可使用</span>') + '</div>' +
    '<div class="meta">' +
      (v.valid_to ? '有效期至 ' + esc(v.valid_to) : '长期有效') +
      (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' 抽中' : '') +
    '</div>' +
    '<div class="sep"></div>' +
    '<div class="ft"><span class="lab">券码</span><code>' + esc(v.code || '-') + '</code>' +
    '<span class="acts">' +
      (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">二维码</button>' : '') +
      '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">复制</button>' +
    '</span></div>' +
    '</div>';
}

async function loadSchoolVouchers() {
  const body = $('vcBody');
  $('vcVeil').classList.add('on');
  body.innerHTML = '<div class="state"><span class="dots">查询中</span></div>';
  $('vcNote').textContent = '';
  try {
    const d = await api('school/vouchers');
    const arr = d.accounts || [];
    const ok = arr.filter(a => !a.error);
    const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
    body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
      '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
      '<span>' + a.vouchers.length + ' 张</span></div>' +
      a.vouchers.map(vcCard).join('')
    ).join('') || '<div class="empty"><div class="big">🎟️</div>还没有抽到券</div>';
    $('vcNote').textContent = total ? total + ' 张券 · ' + ok.filter(a => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
    const errs = arr.filter(a => a.error);
    if (errs.length) {
      body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">查询失败：' +
        errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
    }
    body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
      try { await copyText(b.dataset.copy); toast('券码已复制', 'ok'); }
      catch (e) { toast('复制失败，请手动选择券码', 'err'); }
    });
    // 二维码：券码本体编码为 QR（到店出示扫描），点击切换显示/隐藏
    body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
      const card = b.closest('.vc');
      const old = card.querySelector('.vc-qr');
      if (old) { old.remove(); return; }
      const box = document.createElement('div');
      box.className = 'vc-qr';
      try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
      catch (e) { box.innerHTML = '<span class="note">二维码生成失败：' + esc(e.message) + '</span>'; }
      card.appendChild(box);
    });
  } catch (e) {
    body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
  }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* 成长任务队列。lastQueueSeq 记录本页启动过的队列代次：执行结束后的残留 items
   （running=false 但 seq 停在旧值）不再回写视图——否则扫描结果 3 秒后被上一轮
   队列状态覆盖。 */
let queueTimer = null, lastQueueSeq = 0;
/* 列表归属：'scan' = 「扫描待办」的只读快照，'queue' = 执行队列的实时进度。两者互斥。
   队列快照只在"本轮确实在跑"时接管列表；一轮结束即停止回写——否则上一轮的残留
   items 会在 1 秒内盖掉扫描结果（列表"闪一下就没了"）。 */
let qcOwner = '', queueRunning = false;
const GROWTH_TITLES = {}; // code → 展示名（扫描时从任务列表带出）
// 队列分拣视图：open=未完成（待执行/排队/执行中/失败/跳过），done=已完成。
// 渲染数据留在 qcGroups，切标签纯前端重画，不打接口。
let qcView = 'open', qcGroups = [], qcProgress = null, qcEmptyTitle = '', qcEmptyDesc = '';
$('qcTabs').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-q]');
  if (!b) return;
  qcView = b.dataset.q;
  document.querySelectorAll('#qcTabs .chip').forEach(c => c.classList.toggle('on', c === b));
  drawQueue();
});
$('btnScanAll').onclick = async () => {
  const b = $('btnScanAll');
  if (queueRunning) { toast('队列正在执行中，等本轮结束后再扫描', 'err'); return; }
  qcOwner = 'scan'; // 先夺回列表归属：在途的队列轮询响应不得再覆盖扫描结果
  b.disabled = true; b.textContent = '扫描中…';
  try {
    const d = await api('tasks/scan_all', { method: 'POST' });
    renderQueue(groupItems(d), null, '没有待办任务 🎉', '全部账号的成长任务与开学季活动都已完成，明日再来。');
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '扫描待办'; }
};
$('btnRunQueue').onclick = async () => {
  const conc = Number($('qcConc').value) || 1;
  if (!confirm('扫描全部账号待办并排队执行（账号并发 ' + conc + '，账号内串行）。\n含真实对话的任务耗时较长，确认继续？')) return;
  const b = $('btnRunQueue');
  b.disabled = true; b.textContent = '启动中…';
  try {
    const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
    if (!r.started) { toast(r.message || '没有待办任务', 'ok'); return; }
    lastQueueSeq = r.seq || 0;
    qcOwner = 'queue'; queueRunning = true; // 本轮队列接管列表
    toast('队列已启动：' + r.total + ' 项（并发 ' + conc + '）', 'ok');
    startQueuePolling();
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; b.textContent = '执行全部待办'; }
};
// 扫描结果 → 分组条目（无执行状态）
function groupItems(d) {
  const groups = [];
  for (const a of (d.accounts || [])) {
    const rows = [];
    for (const t of (a.growth || [])) {
      GROWTH_TITLES[t.task_code] = t.title || t.task_code;
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
    }
    for (const t of (a.school || [])) {
      if (t.task_code === 'task_student_verify') continue; // 需真实认证，永不出现在待办
      rows.push({ kind: 'school', code: t.task_code, prog: t.target_count ? t.progress + '/' + t.target_count : '—', status: 'scan' });
    }
    if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
  }
  return groups;
}
const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };
function qrowHTML(it) {
  const isSchool = it.kind === 'school';
  const title = isSchool ? '开学季闭环' : (GROWTH_TITLES[it.code] || it.code);
  const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
  const stWord = it.status === 'scan' ? '待执行' : (ST_WORDS[it.status] || it.status);
  return '<div class="qrow" title="' + esc(it.message || '') + '">' +
    '<span class="code">' + esc(it.code) + '</span>' +
    '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">开学季</span>' : '') + '</span>' +
    '<span class="prog">' + esc(it.prog || '') + '</span>' +
    '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
    '<span class="msg">' + esc(it.message || '') + '</span>' +
    '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
  qcGroups = groups || [];
  qcProgress = progress;
  qcEmptyTitle = emptyTitle || '';
  qcEmptyDesc = emptyDesc || '';
  drawQueue();
}
// drawQueue 按 qcView 分拣渲染：done=本轮执行成功；其余（待执行/排队/执行中/失败/跳过）都算未完成。
function drawQueue() {
  const empty = $('tcEmpty'), list = $('qcList');
  // 两侧计数不随当前视图变：标签上的数字始终是全量口径
  let openN = 0, doneN = 0;
  for (const g of qcGroups) for (const r of g.rows) (r.status === 'done' ? doneN++ : openN++);
  $('qcTabOpen').textContent = '未完成 ' + openN;
  $('qcTabDone').textContent = '已完成 ' + doneN;
  $('qcSummary').textContent = qcGroups.length ? '未完成 ' + openN + ' · 已完成 ' + doneN : '';
  if (!qcGroups.length) {
    empty.style.display = '';
    if (qcEmptyTitle) empty.querySelector('.t').textContent = qcEmptyTitle;
    if (qcEmptyDesc) empty.querySelector('.d').textContent = qcEmptyDesc;
    list.innerHTML = '';
    $('qProg').hidden = true;
    return;
  }
  const groups = qcGroups.map(g => {
    const rows = g.rows.filter(r => qcView === 'done' ? r.status === 'done' : r.status !== 'done');
    return { uid: g.uid, nick: g.nick, rows };
  }).filter(g => g.rows.length);
  updateProgress(qcProgress);
  if (!groups.length) {
    // 数据存在但当前视图为空：给视图相关的空态文案，而不是"还没有扫描过"
    empty.style.display = '';
    empty.querySelector('.t').textContent = qcView === 'open' ? '没有未完成任务 🎉' : '还没有已完成的任务';
    empty.querySelector('.d').textContent = qcView === 'open'
      ? '本轮队列里的任务都已执行完成，切到「已完成」查看结果。'
      : '切到「未完成」查看待办，或点「扫描待办」重新扫描。';
    list.innerHTML = '';
    return;
  }
  empty.style.display = 'none';
  list.innerHTML = groups.map(g =>
    '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' +
    g.rows.length + (qcView === 'done' ? ' 项完成' : ' 项待办') + '</span></header>' +
    g.rows.map(qrowHTML).join('') + '</div>'
  ).join('');
}
function updateProgress(q) {
  if (!q || !q.items) { $('qProg').hidden = true; return; }
  const total = q.items.length;
  const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
  $('qProg').hidden = false;
  $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
  $('qProgText').textContent = (q.running ? '执行中 ' : '已结束 ') + done + ' / ' + total;
}
// 队列状态 → 分组（执行时轮询）
function groupsFromQueue(items) {
  const by = new Map();
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
    by.get(it.uid).rows.push({
      kind: it.kind, code: it.code,
      prog: it.kind === 'school' ? '—' : '',
      status: it.status, message: it.message,
    });
  }
  return Array.from(by.values());
}
async function pollQueueOnce(force) {
  // 没有在跑的队列时不发请求也不回写（任务中心页每秒一拍的 tickLocal 会走到这里）。
  if (!queueRunning && !force) return null;
  let q;
  try { q = await api('tasks/queue'); } catch (e) { return null; }
  if (!q.started) return null;
  // 接管条件：本轮在跑；或本页正在跟踪的那一轮收到 running=false 的收尾那一拍。
  // 历史残留快照（started=true / running=false / 非本页轮次）永不接管视图。
  if (!q.running && !queueRunning) return null;
  // 只渲染本页启动过的那轮队列（刷新页面后不再接管旧队列）。
  if (lastQueueSeq && q.seq !== lastQueueSeq) return null;
  if (qcOwner === 'scan') return null; // 扫描结果优先，挡住在途的队列响应
  qcOwner = 'queue';
  queueRunning = !!q.running;
  renderQueue(groupsFromQueue(q.items || []), q);
  return q;
}
function startQueuePolling() {
  if (queueTimer) clearInterval(queueTimer);
  let miss = 0; // 连续拉取失败计数：接口持续异常时不要 3s 一拍无限打下去
  queueTimer = setInterval(async () => {
    const q = await pollQueueOnce(true);
    if (!q) {
      if (++miss >= 20) {
        clearInterval(queueTimer); queueTimer = null; queueRunning = false;
        toast('队列进度拉取连续失败，已停止轮询', 'err');
      }
      return;
    }
    miss = 0;
    if (!q.running) {
      clearInterval(queueTimer); queueTimer = null;
      toast('任务队列执行结束', 'ok');
      loadSchoolStatus(true);
    }
  }, 3000);
}

/* ── 会话（C）────────────────────────────────────────────────────── */
/* 粘性会话挂在哪个账号上，可单条改绑 / 全量收编。与「换号」（eject）互补：
   eject 是把账号推开被动等会话漂走；这里是主动指定"这条会话下一跳走谁"。 */
function fmtAge(sec) {
  if (sec == null) return '—';
  if (sec < 60) return sec + ' 秒前';
  if (sec < 3600) return Math.floor(sec / 60) + ' 分钟前';
  return Math.floor(sec / 3600) + ' 小时前';
}
function fmtTTL(sec) {
  if (sec == null) return '—';
  if (sec === -1) return '永久';
  if (sec <= 0) return '即将过期';
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm';
  return Math.floor(sec / 3600) + 'h';
}
async function loadSessions() {
  const tb = $('sessBody');
  try {
    const d = await api('sessions');
    sessData = d;
    // 粘性总开关状态：按钮文案 + 列表语义提示。关闭时绑定仍列出（保留态），但注明未生效。
    const on = !!d.sticky_enabled;
    const btn = $('btnSticky');
    btn.textContent = on ? '关闭粘性' : '开启粘性';
    btn.className = on ? 'xs primary' : 'xs';
    const accounts = d.accounts || [];
    const sel = $('sessTarget');
    const prev = sel.value;
    sel.innerHTML = accounts.map(a => '<option value="' + esc(a.uid) + '">' +
      esc(a.nickname || a.uid.slice(0, 8) + '…') + (a.available ? '' : '（不可用）') + '</option>').join('');
    if (prev && accounts.some(a => a.uid === prev)) sel.value = prev;
    const list = d.bindings || [];
    $('sessHint').textContent = !on
      ? '粘性已关闭：请求按权重正常分配；历史绑定保留，开启后立即恢复'
      : list.length
        ? list.length + ' 条绑定 · 同一对话固定走同一账号（账号出问题时自动漂移）'
        : '当前没有粘性绑定（客户端发出带会话 id 的请求后才会建立）';
    if (!list.length) {
      tb.innerHTML = '<tr><td colspan="7"><div class="empty"><div class="big">' + (on ? '没有粘性会话' : '粘性会话未开启') + '</div>' +
        (on ? '客户端带 conversationId 的请求会在网关侧建立"会话 → 账号"绑定' : '点击右上角「开启粘性」后，同一对话会固定走同一账号') + '</div></td></tr>';
      return;
    }
    const nick = k => { const a = accounts.find(x => x.uid === k); return a ? (a.nickname || k.slice(0, 8) + '…') : k.slice(0, 8) + '…'; };
    tb.innerHTML = list.map(b => {
      const opts = accounts.map(a => '<option value="' + esc(a.uid) + '"' + (a.uid === b.uid ? ' selected' : '') + '>' +
        esc(a.nickname || a.uid.slice(0, 8)) + (a.available ? '' : '（不可用）') + '</option>').join('');
      return '<tr title="会话哈希 ' + esc(b.id) + '">' +
        '<td class="mark" aria-hidden="true"><i></i></td>' +
        '<td class="who"><div class="nm" style="font-family:var(--mono);font-size:12.5px">' + esc(b.id) + '</div></td>' +
        '<td>' + (b.kind === 'derived'
          ? '<span class="tag mute" title="客户端未提供会话 id，按 system + 首条用户消息派生">派生</span>'
          : '<span class="tag accent">对话 id</span>') + '</td>' +
        '<td>' + esc(nick(b.uid)) + '</td>' +
        '<td class="num" style="color:var(--ink-3)">' + fmtAge(b.age_sec) + '</td>' +
        '<td class="num" style="color:var(--ink-3)">' + fmtTTL(b.ttl_remain_sec) + '</td>' +
        '<td class="c-acts"><div class="acts"><select data-sel="' + esc(b.id) + '">' + opts + '</select>' +
          '<button class="xs" data-a="rebind" data-s="' + esc(b.id) + '">切到此号</button></div></td>' +
      '</tr>';
    }).join('');
  } catch (e) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
  }
}
$('sessBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a="rebind"]');
  if (!b) return;
  const id = b.dataset.s;
  const sel = $('sessBody').querySelector('select[data-sel="' + id + '"]');
  const uid = sel ? sel.value : '';
  if (!uid) return;
  b.disabled = true;
  try {
    const r = await api('sessions/rebind', { method: 'POST', body: JSON.stringify({ id: id, uid: uid }) });
    toast('已切换 ' + r.rebound + ' 条绑定' + (r.target_available ? '' : '（目标号当前不可用，会先落到别的号）'), 'ok');
    loadSessions(); loadOverview(true);
  } catch (e) { toast(e.message, 'err'); } finally { b.disabled = false; }
});
$('btnSticky').onclick = async () => {
  const on = !!(sessData && sessData.sticky_enabled);
  const next = !on;
  if (next && !confirm('开启粘性会话？\n同一对话将固定走同一个账号（永久保留，账号冷却/锁定/占满时才自动漂移）。\n上游 prompt 缓存命中率会显著提升；代价是单账号的会话集中度变高。')) return;
  if (!next && !confirm('关闭粘性会话？\n之后请求按权重正常分配（每轮可能换号，上游上下文缓存不再命中）；\n历史绑定保留，重新开启后立即恢复。')) return;
  try {
    await api('sticky', { method: 'POST', body: JSON.stringify({ enabled: next }) });
    toast(next ? '粘性会话已开启（已写入配置，重启后保持）' : '粘性会话已关闭（历史绑定保留）', 'ok');
    loadSessions();
  } catch (e) { toast(e.message, 'err'); }
};
$('btnSessRefresh').onclick = () => { loadSessions(); loadOverview(true); };
$('btnAdoptAll').onclick = async () => {
  const uid = $('sessTarget').value;
  if (!uid) return;
  if (!confirm('把当前所有粘性会话都改绑到该账号？\n之后每个会话的下一跳都走它；目标号冷却/锁定/占满时会话会再次漂移。\n（新会话仍按权重分配，要整池只走它请锁定其它号。）')) return;
  try {
    const r = await api('accounts/' + encodeURIComponent(uid) + '/adopt_sessions', { method: 'POST' });
    toast('已改绑 ' + r.bound + ' 条会话' + (r.target_available ? '' : '（目标号当前不可用）'), 'ok');
    loadSessions(); loadOverview(true);
  } catch (e) { toast(e.message, 'err'); }
};

/* ── 开发模式热重载 ──────────────────────────────────────────────── */
/* 后端 DevDir 模式会在页面注入 <meta name="dev-version">（index.html/app.js 任一
   mtime 变化即变）；这里 1.5s 轮询比对，变了就 reload。生产模式无该 meta → 不轮询，
   零开销。CSP script-src 'self'：本段在外链 app.js 里执行，合规。 */
(function () {
  const meta = document.querySelector('meta[name="dev-version"]');
  if (!meta) return; // 生产模式
  let last = meta.getAttribute('content');
  setInterval(async () => {
    try {
      const r = await fetch('/panel/api/dev_version', { headers: { 'Authorization': 'Bearer ' + (localStorage.getItem(LS_KEY) || '') } });
      if (!r.ok) return; // 后端切回生产模式（404）：静默停轮询不成立——interval 已建，让 404 自然跳过
      const v = await r.text();
      if (v && v !== last) {
        last = v;
        // 免打扰：正聚焦输入框（打字/改配置）时延迟一轮，避免打字被 reload 打断
        const el = document.activeElement;
        if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT')) return;
        location.reload();
      }
    } catch (e) { /* 后端重启窗口期：跳过，下拍再试 */ }
  }, 1500);
})();

/* ── 积分构成 ─────────────────────────────────────────────────────── */
/* 一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
   「个人体验版」…），面额从 6 到 1500 不等，且**按次发放**。所以两个任务完成度
   完全一致的账号，余额可能差上千——差别只在包里。
   展示分两层（参照通用积分面板）：账号卡放余额大数字 + 近期到期 TOP2 + 「查看
   全部积分包」入口；点开是单账号逐包弹窗（按到期升序，最快过期的最上面）。 */

// fmtNum 千分位整数（积分总量展示口径：余额/面额都是整数，缩写会丢对比感）。
function fmtNum(n) { return Number(n || 0).toLocaleString('en-US'); }

let pkAccounts = []; // 最近一次查询结果缓存：弹窗打开不再打接口
let pkFetchedAt = 0;

// pkgSorted 该账号的包按到期升序（无到期日沉底）。最快过期的排最前——
// 「先吃过期的」既是使用策略也是提醒优先级。
function pkgSorted(packs) {
  return (packs || []).slice().sort((a, b) => {
    const ea = a.end_time || '9999-12-31', eb = b.end_time || '9999-12-31';
    if (ea !== eb) return ea < eb ? -1 : 1;
    return Number(a.remain || 0) - Number(b.remain || 0);
  });
}

// pkgItemHtml 弹窗里的一行包：名称 + 剩余/总额 + 已用 + 到期 + 剩余比例进度条。
function pkgItemHtml(p) {
  const size = Number(p.size || 0);
  const pct = size > 0 ? Math.min(100, Number(p.remain || 0) / size * 100) : 0;
  const end = (p.end_time || '').slice(0, 10);
  return '<div class="pkg-item">' +
    '<div class="ln1"><span class="nm2" title="' + esc(p.name || '') + '">' + esc(p.name || '(未命名)') + '</span>' +
    '<span class="num">' + fmtNum(p.remain) + ' <span class="of">/ ' + fmtNum(size) + '</span></span></div>' +
    '<div class="ln2"><span>到期 ' + esc(end || '—') + '</span>' +
    '<span class="pkbar" title="剩余 ' + pct.toFixed(1) + '%"><i style="width:' + pct.toFixed(1) + '%"></i></span>' +
    '<span class="used">已用 ' + fmtNum(p.used) + '</span></div>' +
    '</div>';
}

function renderPackages() {
  const list = pkAccounts;
  if (!list.length) {
    $('pkSummary').innerHTML = '<div class="empty">没有账号</div>';
    return;
  }
  $('pkSummary').innerHTML = list.map(a => {
    if (a.error) {
      return '<div class="pk-card"><div class="who"><span class="nm">' +
        esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
        '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
        '<div class="err">查询失败：' + esc(a.error) + '</div></div>';
    }
    const packs = pkgSorted(a.packages || []);
    // 卡上的提醒条自适应数据形态：包带到期日 → 「近期到期」（时间最早优先）；
    // 上游不给到期日（多数账号如此）→ 「即将用完」（剩余比例最低优先，快吃干的
    // 的包先看见）。两者都是"该关注哪几个包"的答案。
    const withEnd = packs.filter(p => p.end_time);
    const expiring = withEnd.length ? withEnd.slice(0, 2)
      : packs.filter(p => Number(p.size || 0) > 0)
          .sort((x, y) => (Number(x.remain || 0) / Number(x.size || 1)) - (Number(y.remain || 0) / Number(y.size || 1)))
          .slice(0, 2);
    const expTitle = withEnd.length ? '近期到期' : '即将用完';
    const expHtml = expiring.length
      ? '<div class="pk-exp-hd">' + expTitle + '</div>' + expiring.map(p => {
        const size = Number(p.size || 0);
        const pct = size > 0 ? Math.min(100, Number(p.remain || 0) / size * 100) : 0;
        const tail = withEnd.length
          ? '<span class="end">' + esc((p.end_time || '').slice(0, 10)) + ' 到期</span>'
          : '<span class="end">剩 ' + pct.toFixed(0) + '%</span>';
        return '<div class="pk-exp"><div class="ln">' +
          '<span class="val">' + fmtNum(p.remain) + ' 积分</span>' +
          '<span class="pknm" title="' + esc(p.name || '') + '">' + esc(p.name || '(未命名)') + '</span>' +
          tail + '</div>' +
          '<div class="pkbar" title="剩余 ' + pct.toFixed(1) + '%"><i style="width:' + pct.toFixed(1) + '%"></i></div></div>';
      }).join('')
      : '<div class="pk-exp-hd">没有可提醒的包</div>';
    return '<div class="pk-card">' +
      '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
      '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
      '<div class="big">' + fmtNum(a.remain) + '</div>' +
      '<div class="pk-sub">' + (a.packages || []).length + ' 个积分包 · 总额 ' + fmtNum(a.size) + '</div>' +
      expHtml +
      '<button type="button" class="pk-more" data-pk="' + esc(a.uid) + '">查看全部积分包 →</button>' +
      '</div>';
  }).join('');

  $('pkNote').textContent = list.length + ' 个账号 · 实时查询上游' +
    (pkFetchedAt ? ' · 更新于 ' + new Date(pkFetchedAt).toLocaleTimeString('zh-CN', { hour12: false }) : '');
}

// openPkgDlg 单账号全部积分包弹窗（数据用缓存，不二次打上游）。
function openPkgDlg(uid) {
  const a = pkAccounts.find(x => x.uid === uid);
  if (!a || a.error) return;
  const packs = pkgSorted(a.packages || []);
  const hasEnd = packs.some(p => p.end_time);
  $('pkgDlgTitle').textContent = '全部积分包 · ' + (a.nickname || a.uid.slice(0, 8));
  $('pkgDlgSub').textContent = '共 ' + packs.length + ' 个积分包 · 余额 ' + fmtNum(a.remain) +
    ' / 总额 ' + fmtNum(a.size) + ' · ' +
    (hasEnd ? '按到期时间升序（最快过期的在最上面）' : '按剩余升序（快用完的在最上面）');
  $('pkgDlgList').innerHTML = packs.length
    ? packs.map(pkgItemHtml).join('')
    : '<div class="empty">该账号没有积分包</div>';
  $('pkgVeil').classList.add('on');
}

function closePkgDlg() { $('pkgVeil').classList.remove('on'); }

async function loadPackages() {
  $('pkSummary').innerHTML = '<div class="empty">查询中…（逐账号向上游实时查询）</div>';
  try {
    const d = await api('packages');
    pkAccounts = d.accounts || [];
    pkFetchedAt = Date.now();
    renderPackages();
  } catch (e) {
    $('pkSummary').innerHTML = '<div class="empty">读取失败：' + esc(e.message) + '</div>';
  }
}

$('pkSummary').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-pk]');
  if (b) openPkgDlg(b.dataset.pk);
});
if ($('btnPk')) $('btnPk').onclick = loadPackages;
if ($('btnPkgClose')) $('btnPkgClose').onclick = closePkgDlg;

