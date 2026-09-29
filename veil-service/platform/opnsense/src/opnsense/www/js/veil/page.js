'use strict';
$(function () {
  const { writable, language } = window.veilUI;
  const tr = (text) => window.VeilTranslations[language]?.[text] || text;
  const el = (id) => document.getElementById('veil-' + id);
  const note = (id, text) => {
    el(id).textContent = text;
    el(id).hidden = !text;
  };
  document.querySelectorAll('#veil-page [data-i18n]').forEach((node) => {
    node.textContent = tr(node.dataset.i18n);
  });
  const request = (q) =>
    q.action === 'connections'
      ? ajaxGet('/api/veil/service/connections', {})
      : ajaxCall('/api/veil/settings/connection', {
          request: JSON.stringify(q),
        });
  const common = window.VeilConnections({
    root: el('connections'),
    language,
    writable,
    request,
  });
  el('settings').hidden = !writable;
  let status = {},
    revision = '',
    saved = '',
    savedEnabled = false,
    busy = false;
  const dirty = () =>
    saved !== el('json').value || savedEnabled !== el('boot-enabled').checked;
  async function api(endpoint, data) {
    const result = await (data === undefined
      ? ajaxGet('/api/veil/' + endpoint, {})
      : ajaxCall('/api/veil/' + endpoint, data));
    if (result.error) throw new Error(result.error.message);
    return result;
  }
  function buttons() {
    el('settings').disabled = busy;
    const running = status.status?.state === 'running';
    el('start').disabled =
      !writable || busy || running || !el('json').value.trim();
    el('start').textContent = dirty() ? tr('Save and start') : tr('Start');
    el('stop').disabled = !writable || busy || !running;
    for (const id of ['save', 'save-apply', 'validate'])
      el(id).disabled =
        busy ||
        !writable ||
        !el('json').value.trim() ||
        (id === 'save' && !dirty());
    el('reload').disabled = !dirty() || busy;
    el('export').disabled = !el('json').value.trim() || busy;
    el('migrate').disabled = !writable || busy;
  }
  async function refresh() {
    try {
      status = await api('service/status');
      el('state').textContent = tr(
        status.status?.state === 'running' ? 'Running' : 'Stopped',
      );
      el('listen').textContent = status.status?.listen || '';
      el('status-json').textContent = JSON.stringify(status, null, 2);
      note(
        'notice',
        status.pending ? tr('Saved changes are waiting to be applied.') : '',
      );
      if (status.status?.error) note('error', status.status.error);
    } catch (error) {
      el('state').textContent = tr('Service unavailable');
      note('error', error.message);
    }
    buttons();
  }
  async function load() {
    const result = await api('settings/get');
    revision = result.revision;
    saved = result.profile;
    savedEnabled = result.enabled;
    el('json').value = saved;
    el('boot-enabled').checked = savedEnabled;
    let legacyClient = false;
    try {
      const cfg = JSON.parse(saved);
      legacyClient = cfg.role === 'client' && !cfg.target;
    } catch (_) {}
    el('migrate').hidden = !legacyClient;
    if (legacyClient) {
      el('standalone').open = true;
      note(
        'notice',
        tr(
          'Move this existing client into the shared connection list. Its connection information is preserved.',
        ),
      );
    }
    buttons();
  }
  async function run(operation) {
    if (busy) return;
    busy = true;
    buttons();
    note('error', '');
    note('feedback', '');
    try {
      await operation();
    } catch (error) {
      note('error', error.message);
    } finally {
      busy = false;
      await refresh();
    }
  }
  function draft() {
    const text = el('json').value;
    if (new Blob([text]).size > 65536)
      throw new Error(tr('Configuration must be smaller than 64 KiB.'));
    const cfg = JSON.parse(text);
    if (!cfg || typeof cfg !== 'object' || Array.isArray(cfg))
      throw new Error(tr('Configuration must be a JSON object.'));
    if (cfg.role === 'client' && !cfg.target)
      throw new Error(
        tr('Import proxy clients into the connection list above.'),
      );
    return { profile: text, enabled: el('boot-enabled').checked ? '1' : '0' };
  }
  async function save() {
    const value = draft();
    const result = await api('settings/save', {
      ...value,
      expected_revision: revision,
    });
    revision = result.revision;
    saved = value.profile;
    savedEnabled = value.enabled === '1';
    note('feedback', tr('Saved.'));
  }
  const confirm = (title, text, action) =>
    common.confirm(title, text, tr('Continue'), () => run(action));
  const apply = async () => {
    await save();
    await api(
      'service/' + (status.status?.state === 'running' ? 'restart' : 'start'),
      { expected_revision: revision },
    );
  };
  el('save').onclick = () => run(save);
  el('save-apply').onclick = () =>
    status.status?.state === 'running'
      ? confirm(
          tr('Apply changes'),
          tr('Applying this instance will reconnect its active streams.'),
          apply,
        )
      : run(apply);
  el('start').onclick = () =>
    run(async () => {
      if (dirty()) await save();
      await api('service/start', { expected_revision: revision });
    });
  el('stop').onclick = () =>
    confirm(
      tr('Stop instance?'),
      tr('Only this standalone instance will stop.'),
      () => api('service/stop', {}),
    );
  el('validate').onclick = () =>
    run(async () => {
      await api('settings/validate', draft());
      note('feedback', tr('Configuration is valid.'));
    });
  el('refresh').onclick = refresh;
  el('reload').onclick = () =>
    dirty()
      ? confirm(
          tr('Discard edits'),
          tr('Discard unsaved edits and reload the saved configuration?'),
          load,
        )
      : run(load);
  el('choose').onclick = () => el('file').click();
  el('file').onchange = async (event) => {
    const file = event.target.files[0];
    event.target.value = '';
    if (!file) return;
    try {
      if (file.size > 65536)
        throw new Error(tr('Configuration must be smaller than 64 KiB.'));
      const cfg = JSON.parse(await file.text());
      if (
        !cfg ||
        typeof cfg !== 'object' ||
        Array.isArray(cfg) ||
        cfg.profile ||
        cfg.kind ||
        (cfg.role === 'client' && !cfg.target)
      )
        throw new Error(
          tr('Import proxy clients into the connection list above.'),
        );
      const replace = () => {
        el('json').value = JSON.stringify(cfg, null, 2);
        buttons();
      };
      if (dirty())
        confirm(
          tr('Replace draft?'),
          tr('Discard unsaved edits and import this file?'),
          replace,
        );
      else replace();
    } catch (error) {
      note('error', error.message);
    }
  };
  el('export').onclick = () => {
    try {
      const value = draft(),
        url = URL.createObjectURL(
          new Blob([value.profile], { type: 'application/json' }),
        );
      const a = document.createElement('a');
      a.href = url;
      a.download = 'veil-instance.json';
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (error) {
      note('error', error.message);
    }
  };
  el('migrate').onclick = () =>
    confirm(
      tr('Move client to connection list'),
      tr(
        'This will reconnect the existing client using the saved settings. Unsaved edits will be discarded.',
      ),
      async () => {
        const result = await request({
          version: 1,
          action: 'connection_migrate',
        });
        if (result.error) throw new Error(result.error.message);
        await load();
        await common.refresh();
      },
    );
  el('json').oninput = el('boot-enabled').onchange = buttons;
  window.addEventListener('beforeunload', (event) => {
    if (dirty() || common.hasDraft()) {
      event.preventDefault();
      event.returnValue = '';
    }
  });
  run(async () => {
    if (writable) await load();
  });
  setInterval(() => {
    if (!busy && !document.hidden) refresh();
  }, 5000);
});
