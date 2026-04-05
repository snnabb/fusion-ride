/* ═══════════════════════════════════════════════════
   FusionRide — SPA 主逻辑
   ═══════════════════════════════════════════════════ */

const API = '/admin/api';
let token = localStorage.getItem('fr_token') || '';
let sseSource = null;
let currentPage = '';
let currentUpstreams = [];

const PLAYBACK_OPTIONS = [
    { value: 'inherit', label: '继承全局默认' },
    { value: 'proxy', label: '代理中转' },
    { value: 'redirect', label: '302 直连' },
];

const GLOBAL_PLAYBACK_OPTIONS = PLAYBACK_OPTIONS.filter(option => option.value !== 'inherit');

const SPOOF_OPTIONS = [
    { value: 'infuse', label: 'Infuse' },
    { value: 'web', label: 'Web' },
    { value: 'client', label: '客户端' },
];

function normalizeSpoofMode(mode) {
    return ['infuse', 'web', 'client'].includes(mode) ? mode : 'infuse';
}

function normalizePlaybackChoice(upstream) {
    if (!upstream || upstream.playbackInherited || !upstream.playbackModeRaw) {
        return 'inherit';
    }
    return upstream.playbackModeRaw;
}

function playbackText(mode) {
    return mode === 'redirect' ? '302 直连' : '代理中转';
}

function spoofText(mode) {
    switch (normalizeSpoofMode(mode)) {
        case 'web':
            return 'Web';
        case 'client':
            return '客户端';
        default:
            return 'Infuse';
    }
}

function playbackBadgeClass(upstream) {
    return upstream && upstream.playbackInherited ? 'inherit' : (upstream?.playbackMode || 'proxy');
}

function playbackBadgeText(upstream) {
    if (!upstream) return playbackText('proxy');
    if (upstream.playbackInherited) {
        return `继承全局 · ${playbackText(upstream.playbackMode || 'proxy')}`;
    }
    return playbackText(upstream.playbackMode || 'proxy');
}

function renderOptions(options, selectedValue) {
    return options.map(option => `
        <option value="${option.value}" ${option.value === selectedValue ? 'selected' : ''}>
            ${option.label}
        </option>
    `).join('');
}

// ── 启动 ──
document.addEventListener('DOMContentLoaded', init);

async function init() {
    // 检查是否需要初始设置
    try {
        const resp = await api('GET', '/needs-setup');
        if (resp.needsSetup) {
            showAuth(true);
            return;
        }
    } catch (e) { /* ignore */ }

    if (token) {
        try {
            await api('GET', '/status');
            showMain();
        } catch (e) {
            token = '';
            localStorage.removeItem('fr_token');
            showAuth(false);
        }
    } else {
        showAuth(false);
    }
}

// ── 认证 ──
function showAuth(isSetup) {
    document.getElementById('auth-screen').style.display = 'flex';
    document.getElementById('main-screen').style.display = 'none';
    document.getElementById('auth-btn-text').textContent = isSetup ? '创建管理员' : '登录';

    document.getElementById('auth-form').onsubmit = async (e) => {
        e.preventDefault();
        const username = document.getElementById('auth-username').value.trim();
        const password = document.getElementById('auth-password').value;
        const errEl = document.getElementById('auth-error');

        if (!username || !password) {
            errEl.textContent = '请输入用户名和密码';
            return;
        }

        try {
            const endpoint = isSetup ? '/setup' : '/login';
            const resp = await fetch(API + endpoint, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ username, password })
            });
            const data = await resp.json();

            if (!resp.ok) throw new Error(data.error || '登录失败');

            token = data.token;
            localStorage.setItem('fr_token', token);
            errEl.textContent = '';
            showMain();
        } catch (err) {
            errEl.textContent = err.message;
            shake(document.getElementById('auth-form'));
        }
    };
}

function showMain() {
    document.getElementById('auth-screen').style.display = 'none';
    document.getElementById('main-screen').style.display = 'flex';

    // 路由
    window.addEventListener('hashchange', handleRoute);
    handleRoute();

    // 退出
    document.getElementById('logout-btn').onclick = () => {
        token = '';
        localStorage.removeItem('fr_token');
        if (sseSource) sseSource.close();
        showAuth(false);
    };

    // SSE
    connectSSE();
}

// ── 路由 ──
function handleRoute() {
    const hash = location.hash || '#/dashboard';
    const page = hash.replace('#/', '') || 'dashboard';

    if (page === currentPage) return;
    currentPage = page;

    // 更新导航高亮
    document.querySelectorAll('.nav-item').forEach(el => {
        el.classList.toggle('active', el.dataset.page === page);
    });

    const content = document.getElementById('content');
    content.innerHTML = '<div style="display:flex;justify-content:center;padding:80px"><div class="spinner"></div></div>';

    const renderers = {
        dashboard: renderDashboard,
        upstreams: renderUpstreams,
        traffic: renderTraffic,
        logs: renderLogs,
        diagnostics: renderDiagnostics,
        settings: renderSettings,
    };

    const render = renderers[page] || renderers.dashboard;
    render(content);
}

// ── 仪表盘 ──
async function renderDashboard(el) {
    try {
        const status = await api('GET', '/status');
        const onlineCount = status.totalOnline || 0;
        const totalCount = status.totalServers || 0;
        const uptimeStr = formatUptime(status.uptime || 0);
        const mappingNote = status.idMappingsNote || '映射会在用户浏览媒体时按需生成';

        currentUpstreams = status.upstreams || [];

        el.innerHTML = `
            <div class="page-header">
                <h1 class="page-title">仪表盘</h1>
                <p class="page-desc">当前运行实例 · ${status.serverName || 'FusionRide'}</p>
            </div>

            <div class="stats-grid">
                <div class="stat-card">
                    <div class="stat-label">在线上游</div>
                    <div class="stat-value green">${onlineCount}</div>
                    <div class="stat-sub">共 ${totalCount} 个上游</div>
                </div>
                <div class="stat-card">
                    <div class="stat-label">ID 映射数量</div>
                    <div class="stat-value accent">${(status.idMappings || 0).toLocaleString()}</div>
                    <div class="stat-sub">${esc(mappingNote)}</div>
                </div>
                <div class="stat-card">
                    <div class="stat-label">运行时间</div>
                    <div class="stat-value" style="font-size:24px">${uptimeStr}</div>
                    <div class="stat-sub">自服务启动以来</div>
                </div>
                <div class="stat-card">
                    <div class="stat-label">全局默认播放模式</div>
                    <div class="stat-value" style="font-size:24px">${playbackText(status.playbackMode || 'proxy')}</div>
                    <div class="stat-sub">上游未单独设置时统一使用</div>
                </div>
            </div>

            <div class="card">
                <div class="card-title">上游状态</div>
                <div class="upstream-list" id="dash-upstream-list">
                    ${currentUpstreams.map(u => `
                        <div class="upstream-card">
                            <div class="status-dot ${u.healthStatus}"></div>
                            <div class="upstream-info">
                                <div class="upstream-name">${esc(u.name)}</div>
                                <div class="upstream-url">${esc(u.url)}</div>
                            </div>
                            <div class="upstream-meta">
                                <span class="badge badge-${playbackBadgeClass(u)}">${esc(playbackBadgeText(u))}</span>
                                <span class="badge badge-${u.healthStatus}">${u.healthStatus}</span>
                            </div>
                        </div>
                    `).join('') || '<div class="empty-state"><div class="empty-state-icon">空</div><div class="empty-state-title">暂无上游</div><p>还没有添加任何 Emby 上游。</p></div>'}
                </div>
            </div>

            <div style="margin-top:24px; padding:16px; background:var(--bg-card); border:1px solid var(--border); border-radius:var(--radius-md);">
                <p style="color:var(--text-secondary); font-size:13px">
                    <strong>Emby 客户端接入地址</strong>
                    <code style="color:var(--accent-light); background:var(--bg-input); padding:2px 8px; border-radius:4px">
                        http://服务器IP:${status.port || 8096}
                    </code>
                </p>
            </div>
        `;
    } catch (err) {
        el.innerHTML = `<div class="empty-state"><div class="empty-state-icon">!</div><div class="empty-state-title">加载失败</div><p>${esc(err.message)}</p></div>`;
    }
}

async function renderUpstreams(el) {
    const upstreams = await api('GET', '/upstreams').catch(() => []);
    currentUpstreams = upstreams;

    el.innerHTML = `
        <div class="page-header" style="display:flex;justify-content:space-between;align-items:flex-start;gap:16px;flex-wrap:wrap">
            <div>
                <h1 class="page-title">上游管理</h1>
                <p class="page-desc">添加 Emby 上游，并配置播放模式、转发方式和请求伪装。</p>
            </div>
            <button class="btn btn-primary" id="add-upstream-btn">+ 添加上游</button>
        </div>

        <div class="upstream-list" id="upstream-list">
            ${upstreams.map(u => upstreamCardHTML(u)).join('') || '<div class="empty-state"><div class="empty-state-icon">空</div><div class="empty-state-title">暂无上游配置</div><p>请先添加至少一个 Emby 上游。</p></div>'}
        </div>
    `;

    document.getElementById('add-upstream-btn').onclick = () => showUpstreamModal();
    el.querySelectorAll('[data-action]').forEach(btn => {
        btn.onclick = () => handleUpstreamAction(btn.dataset.action, parseInt(btn.dataset.id, 10));
    });
}

function upstreamCardHTML(u) {
    const spoofMode = normalizeSpoofMode(u.spoofMode);
    return `
        <div class="upstream-card" id="upstream-${u.id}">
            <div class="status-dot ${u.healthStatus}"></div>
            <div class="upstream-info">
                <div class="upstream-name">${esc(u.name)}</div>
                <div class="upstream-url">${esc(u.url)}</div>
                <div class="upstream-url">账号：${esc(u.username || '未设置')} · 流地址：${esc(u.streamingURL || '未设置')}</div>
            </div>
            <div class="upstream-meta">
                <span class="badge badge-${playbackBadgeClass(u)}">${esc(playbackBadgeText(u))}</span>
                <span class="badge badge-${spoofMode}">${esc(spoofText(spoofMode))}</span>
            </div>
            <div class="upstream-actions">
                <button class="btn btn-secondary btn-sm" data-action="edit" data-id="${u.id}">编辑</button>
                <button class="btn btn-secondary btn-sm" data-action="test" data-id="${u.id}">测试</button>
                <button class="btn btn-secondary btn-sm" data-action="reconnect" data-id="${u.id}">重连</button>
                <button class="btn btn-danger btn-sm" data-action="delete" data-id="${u.id}">删除</button>
            </div>
        </div>
    `;
}

async function handleUpstreamAction(action, id) {
    if (action === 'edit') {
        const upstream = currentUpstreams.find(item => item.id === id);
        if (!upstream) {
            toast('未找到对应上游', 'error');
            return;
        }
        showUpstreamModal(upstream);
        return;
    }

    if (action === 'delete') {
        if (!confirm('确定删除这个上游吗？已生成的 ID 映射不会自动删除。')) return;
        await api('DELETE', `/upstreams/${id}`);
        toast('上游已删除', 'success');
        renderUpstreams(document.getElementById('content'));
        return;
    }

    if (action === 'reconnect') {
        await api('POST', `/upstreams/${id}/reconnect`);
        toast('正在重连上游...', 'info');
        return;
    }

    if (action === 'test') {
        toast('正在测试上游...', 'info');
        const result = await api('POST', `/upstreams/${id}/test`);
        toast(result.online ? `连接成功：${result.message}` : `连接失败：${result.message}`, result.online ? 'success' : 'error');
    }
}

function showAddUpstreamModal() {
    showUpstreamModal();
}

function showUpstreamModal(upstream = null) {
    const isEdit = Boolean(upstream);
    const modal = document.getElementById('modal-content');
    const playbackValue = normalizePlaybackChoice(upstream);
    const spoofValue = normalizeSpoofMode(upstream?.spoofMode);
    const passwordHint = isEdit && upstream?.hasPassword ? '留空则保持当前密码' : '选填';
    const apiKeyHint = isEdit && upstream?.hasAPIKey ? '留空则保持当前 API Key' : 'Emby API Key';

    modal.innerHTML = `
        <div class="modal-title">${isEdit ? '编辑 Emby 上游' : '添加 Emby 上游'}</div>
        <form id="upstream-form" style="display:flex;flex-direction:column;gap:16px">
            <div class="form-group">
                <label>名称</label>
                <input type="text" id="up-name" placeholder="例如主服 / 影院服" value="${esc(upstream?.name || '')}" required>
            </div>
            <div class="form-group">
                <label>地址</label>
                <input type="url" id="up-url" placeholder="https://emby.example.com" value="${esc(upstream?.url || '')}" required>
            </div>
            <div class="form-group">
                <label>用户名（可留空，使用 API Key 也可以）</label>
                <input type="text" id="up-username" placeholder="用户名" value="${esc(upstream?.username || '')}">
            </div>
            <div class="form-group">
                <label>密码</label>
                <input type="password" id="up-password" placeholder="${passwordHint}">
                <div class="field-help">${passwordHint}</div>
            </div>
            <div class="form-group">
                <label>API Key（可选，用于无需账号密码时）</label>
                <input type="text" id="up-apikey" placeholder="${apiKeyHint}">
                <div class="field-help">${apiKeyHint}</div>
            </div>
            <div class="form-group">
                <label>独立流媒体地址（可选）</label>
                <input type="url" id="up-streaming" placeholder="https://stream.example.com" value="${esc(upstream?.streamingURL || '')}">
                <div class="field-help">仅在 302 直连模式下用于替换对外返回的播放地址。</div>
            </div>
            <div class="form-group">
                <label>播放模式</label>
                <select id="up-playback">${renderOptions(PLAYBACK_OPTIONS, playbackValue)}</select>
                <div class="field-help">继承表示使用全局默认播放模式；只有单独指定时才覆盖全局。</div>
            </div>
            <div class="form-group">
                <label>UA 伪装</label>
                <select id="up-spoof">${renderOptions(SPOOF_OPTIONS, spoofValue)}</select>
                <div class="field-help">保留 Infuse、Web、客户端三种模板，用于真实伪装常见终端请求头。</div>
            </div>
            ${isEdit ? `
                <div class="form-group">
                    <label>启用状态</label>
                    <select id="up-enabled">
                        <option value="true" ${upstream?.enabled !== false ? 'selected' : ''}>启用</option>
                        <option value="false" ${upstream?.enabled === false ? 'selected' : ''}>停用</option>
                    </select>
                </div>
            ` : ''}
            <div class="modal-actions">
                <button type="button" class="btn btn-secondary" onclick="closeModal()">取消</button>
                <button type="submit" class="btn btn-primary">${isEdit ? '保存修改' : '添加上游'}</button>
            </div>
        </form>
    `;

    document.getElementById('upstream-form').onsubmit = async (e) => {
        e.preventDefault();
        const payload = {
            name: document.getElementById('up-name').value.trim(),
            url: document.getElementById('up-url').value.trim(),
            username: document.getElementById('up-username').value.trim(),
            playbackMode: document.getElementById('up-playback').value,
            spoofMode: document.getElementById('up-spoof').value,
            streamingURL: document.getElementById('up-streaming').value.trim(),
        };

        const password = document.getElementById('up-password').value;
        const apiKey = document.getElementById('up-apikey').value.trim();
        if (password || !isEdit || !upstream?.hasPassword) {
            payload.password = password;
        }
        if (apiKey || !isEdit || !upstream?.hasAPIKey) {
            payload.apiKey = apiKey;
        }
        if (isEdit) {
            payload.enabled = document.getElementById('up-enabled').value === 'true';
        }

        try {
            if (isEdit) {
                await api('PUT', `/upstreams/${upstream.id}`, payload);
                toast('上游已更新', 'success');
            } else {
                await api('POST', '/upstreams', payload);
                toast('上游已添加，正在进行连通性检查...', 'success');
            }
            closeModal();
            renderUpstreams(document.getElementById('content'));
        } catch (err) {
            toast((isEdit ? '更新失败：' : '添加失败：') + err.message, 'error');
        }
    };

    openModal();
}

async function renderTraffic(el) {
    const data = await api('GET', '/traffic').catch(() => ({ current: [], total: {}, recent: [] }));

    const totalIn = Object.values(data.total || {}).reduce((s, v) => s + (v.bytesIn || 0), 0);
    const totalOut = Object.values(data.total || {}).reduce((s, v) => s + (v.bytesOut || 0), 0);

    el.innerHTML = `
        <div class="page-header">
            <h1 class="page-title">流量监控</h1>
            <p class="page-desc">实时流量统计与历史数据</p>
        </div>

        <div class="stats-grid">
            <div class="stat-card">
                <div class="stat-label">总入站</div>
                <div class="stat-value accent">${formatBytes(totalIn)}</div>
            </div>
            <div class="stat-card">
                <div class="stat-label">总出站</div>
                <div class="stat-value green">${formatBytes(totalOut)}</div>
            </div>
        </div>

        <div class="card" style="margin-bottom:24px">
            <div class="card-title">📊 近 1 小时流量</div>
            <div class="traffic-chart">
                <div class="chart-bars" id="traffic-bars">
                    ${generateChartBars(data.recent || [])}
                </div>
            </div>
        </div>

        <div class="card">
            <div class="card-title">📋 分上游统计</div>
            <div class="table-wrapper">
                <table>
                    <thead><tr><th>上游</th><th>入站</th><th>出站</th></tr></thead>
                    <tbody>
                        ${Object.entries(data.total || {}).map(([id, s]) => `
                            <tr>
                                <td>上游 #${id}</td>
                                <td>${formatBytes(s.bytesIn || 0)}</td>
                                <td>${formatBytes(s.bytesOut || 0)}</td>
                            </tr>
                        `).join('') || '<tr><td colspan="3" style="text-align:center;color:var(--text-muted)">暂无流量数据</td></tr>'}
                    </tbody>
                </table>
            </div>
        </div>
    `;
}

function generateChartBars(data) {
    if (!data.length) return '<div style="display:flex;align-items:center;justify-content:center;height:100%;color:var(--text-muted)">暂无数据</div>';
    const maxVal = Math.max(...data.map(d => (d.bytesOut || 0) + (d.bytesIn || 0)), 1);
    return data.slice(-60).map(d => {
        const h = Math.max(2, ((d.bytesOut || 0) + (d.bytesIn || 0)) / maxVal * 100);
        return `<div class="chart-bar" style="height:${h}%" title="${formatBytes((d.bytesOut||0)+(d.bytesIn||0))}"></div>`;
    }).join('');
}

// ── 日志 ──
async function renderLogs(el) {
    const logs = await api('GET', '/logs?limit=200').catch(() => []);

    el.innerHTML = `
        <div class="page-header" style="display:flex;justify-content:space-between;align-items:flex-start">
            <div>
                <h1 class="page-title">日志</h1>
                <p class="page-desc">系统运行日志</p>
            </div>
            <div style="display:flex;gap:8px">
                <button class="btn btn-secondary btn-sm" id="log-download">📥 下载</button>
                <button class="btn btn-danger btn-sm" id="log-clear">🗑️ 清空</button>
                <button class="btn btn-secondary btn-sm" id="log-refresh">🔄 刷新</button>
            </div>
        </div>

        <div class="log-container" id="log-list">
            ${logs.reverse().map(l => `
                <div class="log-entry">
                    <span class="log-time">${new Date(l.time).toLocaleTimeString()}</span>
                    <span class="log-level ${l.level}">${l.level}</span>
                    <span class="log-msg">${esc(l.message)}</span>
                </div>
            `).join('') || '<div style="text-align:center;color:var(--text-muted);padding:40px">暂无日志</div>'}
        </div>
    `;

    document.getElementById('log-refresh').onclick = () => renderLogs(el);
    document.getElementById('log-clear').onclick = async () => {
        if (!confirm('确定清空所有日志？')) return;
        await api('DELETE', '/logs');
        toast('日志已清空', 'success');
        renderLogs(el);
    };
    document.getElementById('log-download').onclick = () => {
        window.open(API + '/logs/download?token=' + token);
    };
}

// ── 诊断 ──
async function renderDiagnostics(el) {
    el.innerHTML = `
        <div class="page-header">
            <h1 class="page-title">诊断</h1>
            <p class="page-desc">上游连通性检测与延迟测试</p>
        </div>
        <div style="display:flex;justify-content:center;padding:60px"><div class="spinner"></div></div>
    `;

    try {
        const data = await api('GET', '/diagnostics');

        el.innerHTML = `
            <div class="page-header" style="display:flex;justify-content:space-between;align-items:flex-start">
                <div>
                    <h1 class="page-title">诊断</h1>
                    <p class="page-desc">上游连通性检测与延迟测试</p>
                </div>
                <button class="btn btn-secondary btn-sm" onclick="renderDiagnostics(document.getElementById('content'))">🔄 重新检测</button>
            </div>

            ${(data.upstreams || []).map(u => `
                <div class="diag-card">
                    <div class="status-dot ${u.online ? 'online' : 'offline'}"></div>
                    <div class="upstream-info">
                        <div class="upstream-name">${esc(u.name)}</div>
                        <div class="upstream-url">${esc(u.url)}</div>
                        <div style="font-size:12px;color:var(--text-muted);margin-top:4px">
                            ${esc(u.message)} · 伪装: ${u.spoofMode}
                        </div>
                    </div>
                    <div class="diag-latency ${u.latency < 500 ? 'fast' : u.latency < 2000 ? 'mid' : 'slow'}">
                        ${u.latency}ms
                    </div>
                </div>
            `).join('') || '<div class="empty-state"><div class="empty-state-icon">🔌</div><div class="empty-state-title">暂无上游</div></div>'}
        `;
    } catch (err) {
        el.innerHTML = `<div class="empty-state"><div class="empty-state-icon">⚠️</div><div class="empty-state-title">诊断失败</div><p>${esc(err.message)}</p></div>`;
    }
}

// ── 设置 ──
async function renderSettings(el) {
    const settings = await api('GET', '/settings').catch(() => ({}));

    el.innerHTML = `
        <div class="page-header">
            <h1 class="page-title">设置</h1>
            <p class="page-desc">修改服务名称、默认播放模式和管理员信息。</p>
        </div>

        <div class="card">
            <div class="settings-section">
                <h3>服务</h3>
                <div class="settings-row">
                    <div class="settings-label">服务名称</div>
                    <div>
                        <input type="text" id="set-name" value="${esc(settings.serverName || 'FusionRide')}">
                    </div>
                </div>
                <div class="settings-row">
                    <div class="settings-label">端口</div>
                    <div>
                        <input type="number" id="set-port" value="${settings.port || 8096}" disabled>
                        <div class="settings-help">端口由配置文件控制，当前页面不允许直接修改。</div>
                    </div>
                </div>
            </div>

            <div class="settings-section">
                <h3>播放</h3>
                <div class="settings-row">
                    <div class="settings-label">全局默认播放模式</div>
                    <div>
                        <select id="set-playback">${renderOptions(GLOBAL_PLAYBACK_OPTIONS, settings.playbackMode || 'proxy')}</select>
                        <div class="settings-help">当上游未单独指定播放模式时，统一使用这里的设置。</div>
                    </div>
                </div>
            </div>

            <div class="settings-section">
                <h3>安全</h3>
                <div class="settings-row">
                    <div class="settings-label">管理员密码</div>
                    <div>
                        <button class="btn btn-secondary btn-sm" id="change-pwd-btn">修改管理员密码</button>
                    </div>
                </div>
            </div>

            <div style="display:flex;justify-content:flex-end;margin-top:24px">
                <button class="btn btn-primary" id="save-settings-btn">保存设置</button>
            </div>
        </div>
    `;

    document.getElementById('save-settings-btn').onclick = async () => {
        try {
            await api('PUT', '/settings', {
                serverName: document.getElementById('set-name').value,
                playbackMode: document.getElementById('set-playback').value,
            });
            toast('设置已保存', 'success');
        } catch (err) {
            toast('保存失败：' + err.message, 'error');
        }
    };

    document.getElementById('change-pwd-btn').onclick = () => {
        const modal = document.getElementById('modal-content');
        modal.innerHTML = `
            <div class="modal-title">修改管理员密码</div>
            <form id="pwd-form" style="display:flex;flex-direction:column;gap:16px">
                <div class="form-group">
                    <label>旧密码</label>
                    <input type="password" id="pwd-old" required>
                </div>
                <div class="form-group">
                    <label>新密码</label>
                    <input type="password" id="pwd-new" required>
                </div>
                <div class="modal-actions">
                    <button type="button" class="btn btn-secondary" onclick="closeModal()">取消</button>
                    <button type="submit" class="btn btn-primary">更新密码</button>
                </div>
            </form>
        `;
        document.getElementById('pwd-form').onsubmit = async (e) => {
            e.preventDefault();
            try {
                await api('POST', '/password', {
                    oldPassword: document.getElementById('pwd-old').value,
                    newPassword: document.getElementById('pwd-new').value,
                });
                closeModal();
                toast('密码已更新', 'success');
            } catch (err) {
                toast('修改失败：' + err.message, 'error');
            }
        };
        openModal();
    };
}

function connectSSE() {
    if (sseSource) sseSource.close();
    sseSource = new EventSource(API + '/traffic/stream?token=' + token);

    sseSource.onmessage = (e) => {
        try {
            const data = JSON.parse(e.data);
            if (data.event === 'upstream_added' || data.event === 'upstream_removed' || data.event === 'upstream_updated') {
                if (currentPage === 'dashboard' || currentPage === 'upstreams') {
                    handleRoute(); // 重新渲染当前页
                }
            }
        } catch (err) { /* ignore */ }
    };

    sseSource.onerror = () => {
        setTimeout(connectSSE, 5000);
    };
}

// ── API 工具 ──
async function api(method, path, body) {
    const opts = {
        method,
        headers: {
            'Content-Type': 'application/json',
            'Authorization': 'Bearer ' + token,
        },
    };
    if (body) opts.body = JSON.stringify(body);

    const resp = await fetch(API + path, opts);
    const raw = await resp.text();
    let data = {};

    if (raw) {
        try {
            data = JSON.parse(raw);
        } catch (err) {
            data = { error: raw };
        }
    }

    if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
    return data;
}

function toast(msg, type = 'info') {
    const container = document.getElementById('toast-container');
    const el = document.createElement('div');
    el.className = `toast ${type}`;
    el.textContent = msg;
    container.appendChild(el);
    setTimeout(() => { el.style.opacity = '0'; el.style.transform = 'translateX(100%)'; setTimeout(() => el.remove(), 300); }, 4000);
}

function openModal() {
    document.getElementById('modal-overlay').style.display = 'flex';
}

function closeModal() {
    document.getElementById('modal-overlay').style.display = 'none';
}

document.getElementById('modal-overlay').addEventListener('click', (e) => {
    if (e.target === e.currentTarget) closeModal();
});

function shake(el) {
    el.style.animation = 'none';
    el.offsetHeight; // reflow
    el.style.animation = 'shake 0.5s ease';
}

function esc(s) {
    if (!s) return '';
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}

function formatUptime(seconds) {
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    if (d > 0) return `${d}天 ${h}时`;
    if (h > 0) return `${h}时 ${m}分`;
    return `${m}分`;
}

function formatBytes(bytes) {
    if (bytes === 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(1024));
    return (bytes / Math.pow(1024, i)).toFixed(1) + ' ' + units[i];
}

// 暴露给 inline onclick
window.closeModal = closeModal;
window.renderDiagnostics = renderDiagnostics;
