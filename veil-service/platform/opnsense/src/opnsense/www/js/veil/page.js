'use strict';

$(function () {
    const writable = window.veilUI.writable;
    const tr = text => window.veilUI.catalog[text] || text;
    const el = id => document.getElementById('veil-' + id);
    const note = (id, text) => { el(id).textContent = text; el(id).hidden = !text; };
    document.querySelectorAll('#veil-page [data-i18n]').forEach(node => { node.textContent = tr(node.dataset.i18n); });
    el('settings').hidden = !writable;
    let current = {}, busy = false, saved = '', revision = '', savedEnabled = false;

    async function api(endpoint, data) {
        const result = await (data === undefined ? ajaxGet('/api/veil/' + endpoint, {}) : ajaxCall('/api/veil/' + endpoint, data));
        if (result.error) {
            const labels = {
                conflict: 'The saved configuration changed elsewhere. Reload it before saving again.',
                invalid_config: 'Configuration validation failed.',
                unavailable: 'The control service is unavailable.',
                no_config: 'Import and save a configuration first.'
            };
            throw new Error(tr(labels[result.error.code] || 'Operation failed.') + ' ' + result.error.message);
        }
        return result;
    }

    function buttons() {
        const running = current.status?.state === 'running';
        el('start').disabled = busy || !writable || running || !current.configured;
        el('stop').disabled = busy || !writable || !running;
        el('apply').disabled = busy || !writable || !current.configured || !current.pending || !running;
        el('save').disabled = busy || !writable || (saved === el('json').value && savedEnabled === el('enabled').checked);
        el('validate').disabled = busy || !writable || !el('json').value.trim();
        for (const id of ['choose', 'file', 'json', 'enabled', 'reload']) el(id).disabled = busy || !writable;
        for (const id of ['protocol', 'address']) el(id).disabled = busy || !writable || !el('json').value.trim();
    }

    async function refresh() {
        try {
            current = await api('service/status');
            const status = current.status || {};
            el('state').textContent = tr(status.state === 'running' ? 'Running' : 'Stopped');
            el('state').className = 'label ' + (status.state === 'running' ? 'label-success' : 'label-default');
            el('role').textContent = status.role ? tr(status.role === 'client' ? 'Client' : 'Server') : '—';
            el('listen').textContent = status.listen || '—';
            el('count').textContent = status.stats?.completed || 0;
            note('notice', !current.configured ? tr('Import a configuration to get started.') : current.pending && status.state === 'running' ? tr('A saved configuration is waiting to be applied. Applying it will disconnect current connections.') : '');
            if (status.error || status.last_connection_error) note('error', status.error || status.last_connection_error);
        } catch (error) {
            current = {};
            el('state').textContent = tr('Service unavailable');
            el('state').className = 'label label-danger';
            note('error', error.message || tr('Operation failed.'));
        }
        buttons();
    }

    async function run(operation) {
        if (busy) return;
        busy = true; buttons(); note('error', ''); note('feedback', '');
        try { await operation(); }
        catch (error) { note('error', error.message || tr('Operation failed.')); }
        finally { busy = false; await refresh(); }
    }

    function profile() {
        if (new Blob([el('json').value]).size > 65536) throw new Error(tr('Configuration must be smaller than 64 KiB.'));
        let value;
        try { value = JSON.parse(el('json').value); }
        catch (_) { throw new Error(tr('Invalid JSON. Check the configuration before saving.')); }
        if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(tr('The configuration must be a JSON object.'));
        return value;
    }

    function syncFields() {
        try {
            const cfg = profile();
            el('listener').hidden = cfg.role !== 'client' || !!cfg.target;
            el('protocol').value = cfg.inbound || 'socks';
            el('address').value = cfg.listen || '127.0.0.1:1080';
        } catch (_) { /* Keep an invalid draft editable. */ }
    }

    function draft() { profile(); return {profile: el('json').value, enabled: el('enabled').checked ? '1' : '0'}; }
    async function load() {
        const settings = await api('settings/get');
        revision = settings.revision;
        saved = settings.profile;
        savedEnabled = settings.enabled;
        el('json').value = saved;
        el('enabled').checked = savedEnabled;
        syncFields(); buttons();
    }

    el('refresh').onclick = refresh;
    el('start').onclick = () => run(() => api('service/start', {expected_revision: current.revision}));
    el('stop').onclick = () => run(() => api('service/stop', {}));
    el('apply').onclick = () => {
        const expected = current.revision;
        stdDialogConfirm(
            tr('Apply saved configuration?'),
            tr('Current connections will be disconnected. Unsaved edits in the editor are not applied.'),
            tr('Apply and restart'), tr('Cancel'),
            () => run(() => api('service/restart', {expected_revision: expected}))
        );
    };
    el('choose').onclick = () => el('file').click();
    el('file').onchange = () => run(async () => {
        const file = el('file').files[0];
        if (!file) return;
        if (file.size > 65536) throw new Error(tr('Configuration must be smaller than 64 KiB.'));
        el('filename').textContent = file.name;
        el('json').value = await file.text();
        syncFields();
        await api('settings/validate', draft());
        note('feedback', tr('Configuration imported and validated. Save it when ready.'));
    });
    el('save').onclick = () => run(async () => {
        const result = await api('settings/save', {...draft(), expected_revision: revision});
        revision = result.revision;
        saved = el('json').value;
        savedEnabled = el('enabled').checked;
        note('feedback', tr('Configuration saved.'));
    });
    el('validate').onclick = () => run(async () => { await api('settings/validate', draft()); note('feedback', tr('Configuration is valid.')); });
    el('reload').onclick = () => {
        if (saved !== el('json').value || savedEnabled !== el('enabled').checked) {
            stdDialogConfirm(tr('Reload saved configuration'), tr('Discard unsaved edits and reload the saved configuration?'), tr('Reload saved configuration'), tr('Cancel'), () => run(load));
        } else run(load);
    };
    el('json').oninput = () => { syncFields(); buttons(); };
    el('enabled').onchange = buttons;
    for (const id of ['protocol', 'address']) el(id).onchange = () => {
        try {
            const cfg = profile(); cfg.inbound = el('protocol').value; cfg.listen = el('address').value;
            el('json').value = JSON.stringify(cfg, null, 2); buttons();
        } catch (error) { note('error', error.message); }
    };
    run(async () => { if (writable) await load(); });
    setInterval(() => { if (!busy && !document.hidden) refresh(); }, 5000);
});
