'use strict';
// Shared connection journeys; hosts supply authenticated control and file dialogs.
window.VeilConnections = function (options) {
  const bind = (fn, self) => fn.bind(self);
  const _ = (text) =>
    options.translate
      ? options.translate(text)
      : (window.VeilTranslations?.[options.language || 'en'] || {})[text] ||
        text;
  function E(tag, attributes = {}, children = []) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(attributes)) {
      if (typeof value === 'function') node.addEventListener(key, value);
      else if (value !== false && value != null)
        node.setAttribute(key, value === true ? '' : value);
    }
    for (const child of [children].flat(Infinity))
      if (child != null)
        node.append(
          child instanceof Node
            ? child
            : document.createTextNode(String(child)),
        );
    return node;
  }
  const dom = {
    content: (node, children) => node.replaceChildren(...[children].flat()),
  };
  const request = (action, values = {}) =>
    options.request({ version: 1, action, ...values });
  const list = () => request('connections');
  const get = (id) => request('connection_get', { id });
  const exportConnection = (id) => request('connection_export', { id });
  const save = (profile, expected_revision, apply, relay) =>
    request('connection_save', {
      profile,
      expected_revision,
      apply,
      ...(relay ? { relay } : {}),
    });
  const start = (id, expected_revision) =>
    request('connection_start', { id, expected_revision });
  const stop = (id, expected_revision) =>
    request('connection_stop', { id, expected_revision });
  const remove = (id, expected_revision) =>
    request('connection_delete', { id, expected_revision });
  const probe = (id) => request('connection_test', { id });
  let dialog;
  const ui = {
    hideModal: () => {
      dialog?.close();
      dialog?.remove();
      dialog = null;
      view.editorOpen = false;
      view.cancelEditor = null;
      view.hasDraft = () => false;
    },
    showModal: (title, children) => {
      ui.hideModal();
      view.editorOpen = true;
      dialog = E(
        'dialog',
        { class: 'veil-dialog', 'data-theme': options.theme?.() || '' },
        [E('h2', {}, title), ...children],
      );
      dialog.addEventListener('cancel', (event) => {
        event.preventDefault();
        if (view.cancelEditor) view.cancelEditor();
        else ui.hideModal();
      });
      document.body.append(dialog);
      dialog.showModal();
    },
    addNotification: (_title, content) => {
      const target = dialog
        ? dialog.querySelector('[role=alert]') ||
          dialog.prepend(E('p', { role: 'alert', class: 'veil-error' })) ||
          dialog.querySelector('[role=alert]')
        : view.notice;
      target.replaceChildren(content);
      target.scrollIntoView({ block: 'nearest' });
    },
  };
  function checked(r) {
    if (r.error) throw new Error(r.error.message);
    return r;
  }
  function button(label, action, primary) {
    return E(
      'button',
      {
        type: 'button',
        class:
          'cbi-button ' +
          (primary ? 'cbi-button-positive' : 'cbi-button-neutral'),
        click: action,
      },
      label,
    );
  }
  function field(label, input, help) {
    return E('label', { class: 'veil-field' }, [
      E('span', {}, label),
      input,
      help ? E('small', {}, help) : '',
    ]);
  }
  function input(value, type) {
    return E('input', {
      class: 'cbi-input-text',
      type: type || 'text',
      value: value || '',
      autocomplete: 'off',
    });
  }
  function select(options, value) {
    var s = E(
      'select',
      { class: 'cbi-input-select' },
      options.map(function (o) {
        return E('option', { value: o[0] }, o[1]);
      }),
    );
    s.value = value;
    return s;
  }
  function splitAddress(address) {
    var m = /^(?:\[([^\]]+)\]|([^:]+)):(\d+)$/.exec(address || '');
    return m ? [m[1] || m[2], m[3]] : ['', '443'];
  }
  function address(host, port) {
    return (host.indexOf(':') >= 0 ? '[' + host + ']' : host) + ':' + port;
  }

  const view = {
    load: function () {
      this.writable = options.writable !== false;
      return list().then(checked);
    },
    refresh: function (force) {
      return list()
        .then(checked)
        .then(
          bind(function (r) {
            this.rows = r.connections || [];
            const page = options.root.querySelector('.veil-page');
            if (page && options.theme) page.dataset.theme = options.theme();
            if (force || !this.connections.querySelector('details[open]'))
              this.draw();
          }, this),
        )
        .catch(
          bind(function (e) {
            this.notice.textContent = e.message;
          }, this),
        );
    },
    action: function (operation) {
      this.notice.textContent = '';
      return Promise.resolve()
        .then(operation)
        .then(checked)
        .catch(function (e) {
          ui.addNotification(null, E('p', {}, e.message), 'error');
        })
        .finally(() => this.refresh(true));
    },
    confirm: function (title, message, label, action) {
      ui.showModal(title, [
        E('p', {}, message),
        E('div', { class: 'veil-actions veil-right' }, [
          button(_('Cancel'), ui.hideModal),
          button(
            label,
            function () {
              ui.hideModal();
              action();
            },
            true,
          ),
        ]),
      ]);
    },
    diagnostics: function (row) {
      const formatTime = (value) =>
        value
          ? new Date(value).toLocaleString(
              options.language === 'zh' ? 'zh-CN' : 'en-US',
            )
          : '—';
      const stage = (value) =>
        ({
          'tunnel dial': _('Connect to server'),
          'TLS handshake': _('TLS handshake'),
          authentication: _('Authentication'),
          'open target': _('Open destination'),
          'target dial': _('Connect to destination'),
          inbound: _('Local proxy'),
          idle: _('Idle timeout'),
          'tunnel read': _('Read tunnel'),
          'tunnel write': _('Write tunnel'),
          'local read': _('Read local socket'),
          'local write': _('Write local socket'),
        })[value] || value;
      const hop = (name, snapshot) => {
        const d = snapshot.diagnostics || {},
          stats = snapshot.stats || {},
          pool = d.pool;
        const metric = (name, value) =>
          E('div', {}, [
            E('dt', {}, name),
            E('dd', {}, value == null ? '—' : String(value)),
          ]);
        return E('section', { class: 'veil-diagnostic-hop' }, [
          E('h3', {}, name),
          E('dl', { class: 'veil-metrics' }, [
            metric(_('Started at'), formatTime(d.started_at)),
            metric(_('Active local connections'), stats.active_connections),
            metric(_('Connection limit per listener'), d.connection_limit),
            metric(_('Active streams'), stats.active_streams),
            metric(_('Rejected connections'), stats.rejected),
            metric(_('Failed operations'), stats.failed),
            ...(pool
              ? [
                  metric(
                    _('Tunnel connections / limit'),
                    pool.total + ' / ' + pool.limit,
                  ),
                  metric(_('Idle tunnels'), pool.idle),
                  metric(
                    _('Opening a tunnel'),
                    pool.dialing ? _('Yes') : _('No'),
                  ),
                ]
              : []),
          ]),
          E('h4', {}, _('Recent errors')),
          ...(d.recent?.length
            ? [...d.recent]
                .reverse()
                .map((event) =>
                  E('article', { class: 'veil-diagnostic-event' }, [
                    E('strong', {}, stage(event.stage)),
                    E(
                      'small',
                      {},
                      formatTime(event.at) +
                        ' · ' +
                        _('Occurrences') +
                        ': ' +
                        event.count,
                    ),
                    E('p', {}, event.message),
                    event.count > 1
                      ? E(
                          'small',
                          {},
                          _('First occurrence') +
                            ': ' +
                            formatTime(event.first_at),
                        )
                      : '',
                  ]),
                )
            : [
                E(
                  'p',
                  { class: 'veil-muted' },
                  _('No recorded errors in this run.'),
                ),
              ]),
        ]);
      };
      const report = {
        version: 1,
        captured_at: new Date().toISOString(),
        connection: row,
      };
      const refresh = button(_('Refresh'), () =>
        list()
          .then(checked)
          .then((result) => {
            const latest = (result.connections || []).find(
              (r) => r.id === row.id,
            );
            if (!latest) throw new Error(_('Connection no longer exists.'));
            this.rows = result.connections;
            this.diagnostics(latest);
          })
          .catch((e) => ui.addNotification(null, E('p', {}, e.message))),
      );
      const download = button(_('Export diagnostics'), () => {
        const url = URL.createObjectURL(
          new Blob([JSON.stringify(report, null, 2)], {
            type: 'application/json',
          }),
        );
        E('a', { href: url, download: row.id + '-diagnostics.json' }).click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      });
      ui.showModal(_('Diagnostics') + ' · ' + row.name, [
        E(
          'p',
          { class: 'veil-muted' },
          _(
            'Historical errors do not indicate that the connection is currently unavailable. Test the connection to check it now.',
          ),
        ),
        hop(_('Exit tunnel'), row),
        row.relay ? hop(_('Relay tunnel'), row.relay) : '',
        E(
          'p',
          { class: 'veil-muted' },
          _(
            'The report includes addresses and error messages, but no connection keys. Remote server logs must be checked on that server.',
          ),
        ),
        E('details', {}, [
          E('summary', {}, _('Raw status')),
          E(
            'pre',
            { class: 'veil-diagnostic-raw' },
            JSON.stringify(report, null, 2),
          ),
        ]),
        E('div', { class: 'veil-actions veil-right' }, [
          download,
          refresh,
          button(_('Close'), ui.hideModal),
        ]),
      ]);
    },
    draw: function () {
      if (options.onChange) options.onChange(this.rows);
      var connections = this.rows.filter(function (r) {
        return r.kind === 'connection';
      });
      var relays = this.rows.filter(function (r) {
        return r.kind === 'relay';
      });
      var node = bind(function (r) {
        var status =
          r.kind === 'relay'
            ? _('Relay profile')
            : r.state === 'running'
              ? _('Proxy started')
              : r.error
                ? _('Start failed')
                : _('Stopped');
        var testing = this.testing && this.testing[r.id];
        var testText = testing
          ? _('Testing…')
          : r.probe
            ? r.probe.ok
              ? r.probe.milliseconds + ' ms'
              : _('Failed')
            : _('Not tested');
        var test = button(
          _('Test Google'),
          bind(function () {
            this.testing = this.testing || {};
            this.testing[r.id] = true;
            this.draw();
            test.disabled = true;
            test.textContent = _('Testing…');
            this.action(function () {
              return probe(r.id);
            }).finally(
              bind(function () {
                delete this.testing[r.id];
                this.draw();
              }, this),
            );
          }, this),
        );
        test.disabled = !this.writable || testing;
        var edit = button(
          _('Edit'),
          bind(function () {
            this.action(
              bind(function () {
                return get(r.id)
                  .then(checked)
                  .then(
                    bind(function (v) {
                      this.edit(v.profile, r.revision);
                      return v;
                    }, this),
                  );
              }, this),
            );
          }, this),
        );
        edit.disabled = !this.writable;
        var applyPending = button(
          _('Apply changes'),
          bind(function () {
            this.confirm(
              _('Apply changes'),
              _('Apply saved changes now? Affected connections may reconnect.'),
              _('Apply changes'),
              bind(function () {
                this.action(function () {
                  return get(r.id)
                    .then(checked)
                    .then(function (v) {
                      return save(v.profile, r.revision, true, null);
                    });
                });
              }, this),
            );
          }, this),
        );
        applyPending.disabled = !this.writable;
        var menu = E('details', { class: 'veil-more' }, [
          E('summary', {}, _('More')),
        ]);
        var exportButton = button(_('Export'), function () {
          if (options.exportFile) {
            options
              .exportFile(r.id)
              .catch((e) => ui.addNotification(null, E('p', {}, e.message)));
            return;
          }
          exportConnection(r.id)
            .then(checked)
            .then(function (v) {
              var url = URL.createObjectURL(
                new Blob(
                  [
                    JSON.stringify(
                      { version: 1, profile: v.profile, relay: v.relay },
                      null,
                      2,
                    ),
                  ],
                  { type: 'application/json' },
                ),
              );
              var a = E('a', { href: url, download: r.id + '.json' });
              a.click();
              setTimeout(function () {
                URL.revokeObjectURL(url);
              }, 1000);
            })
            .catch(function (e) {
              ui.addNotification(null, E('p', {}, e.message), 'error');
            });
        });
        exportButton.disabled = !this.writable;
        var duplicate = button(
          _('Duplicate'),
          bind(function () {
            this.action(
              bind(function () {
                return get(r.id)
                  .then(checked)
                  .then(
                    bind(function (v) {
                      v.profile.id =
                        'node-' +
                        Date.now().toString(36) +
                        Math.random().toString(36).slice(2, 6);
                      v.profile.name += ' (' + _('Copy') + ')';
                      v.profile.enabled = false;
                      v.profile.inlets =
                        v.profile.kind === 'connection'
                          ? [
                              {
                                protocol: 'mixed',
                                listen: '127.0.0.1:' + this.nextPort(),
                              },
                            ]
                          : [];
                      this.edit(v.profile, '');
                      return v;
                    }, this),
                  );
              }, this),
            );
          }, this),
        );
        duplicate.disabled = !this.writable;
        var del = button(
          _('Delete'),
          bind(function () {
            this.confirm(
              _('Delete connection?'),
              _(
                'Delete this profile and stop its listeners? Relays in use cannot be deleted.',
              ),
              _('Delete'),
              bind(function () {
                this.action(function () {
                  return remove(r.id, r.revision);
                });
              }, this),
            );
          }, this),
        );
        del.disabled = !this.writable;
        const diagnostic = button(_('Diagnostics'), () => this.diagnostics(r));
        diagnostic.dataset.readonly = 'true';
        menu.appendChild(
          E('div', { class: 'veil-actions' }, [
            diagnostic,
            exportButton,
            duplicate,
            del,
          ]),
        );
        var route = r.relay_id
          ? E('div', { class: 'veil-route' }, [
              E('span', {}, _('Via') + ' ' + r.relay_name),
              E('code', {}, r.relay_server),
              E('span', {}, '→ ' + r.server),
            ])
          : E('span', { class: 'veil-muted' }, _('Direct connection'));
        var endpoints = (r.inlets || []).map(function (i) {
          return E('div', { class: 'veil-endpoint' }, [
            E(
              'span',
              {},
              i.protocol === 'mixed'
                ? 'SOCKS5 + HTTP'
                : i.protocol === 'socks'
                  ? 'SOCKS5'
                  : 'HTTP',
            ),
            E('code', {}, i.listen),
          ]);
        });
        return E(
          'article',
          {
            class: 'veil-card',
            'data-id': r.id,
            'data-state': r.state,
            'data-enabled': String(r.enabled),
          },
          [
            E('div', { class: 'veil-card-head' }, [
              E('div', {}, [
                E('h3', {}, r.name),
                E('code', { class: 'veil-server' }, r.server),
              ]),
              E(
                'span',
                {
                  class:
                    'veil-badge ' +
                    (r.state === 'running' ? 'veil-running' : ''),
                },
                status,
              ),
            ]),
            r.kind === 'connection' ? route : '',
            r.kind === 'connection'
              ? E(
                  'div',
                  { class: 'veil-endpoints' },
                  endpoints.length
                    ? endpoints
                    : [E('span', {}, _('No proxy listeners'))],
                )
              : E(
                  'p',
                  { class: 'veil-muted' },
                  _(
                    'Shared relay settings. Test Google on a complete connection that uses this relay.',
                  ),
                ),
            r.pending
              ? E(
                  'p',
                  { class: 'veil-notice' },
                  _(
                    'Saved changes are waiting to be applied. Tests use saved settings; proxy listeners still use running settings.',
                  ),
                )
              : '',
            r.error ? E('p', { class: 'veil-error' }, r.error) : '',
            r.kind === 'connection'
              ? E('div', { class: 'veil-test-result' }, [
                  E('span', {}, _('Google HTTPS') + ': '),
                  E('strong', {}, testText),
                  r.probe
                    ? E('small', {}, new Date(r.probe.at).toLocaleTimeString())
                    : '',
                  r.probe && r.probe.error
                    ? E('p', { class: 'veil-error' }, r.probe.error)
                    : '',
                ])
              : '',
            E('div', { class: 'veil-actions' }, [
              r.kind === 'connection'
                ? button(
                    r.state === 'running' ? _('Stop') : _('Start'),
                    () => {
                      const act = () =>
                        this.action(() =>
                          (r.state === 'running' ? stop : start)(
                            r.id,
                            r.revision,
                          ),
                        );
                      if (r.state === 'running')
                        this.confirm(
                          _('Stop connection?'),
                          _(
                            'Only this connection and its proxy listeners will stop.',
                          ),
                          _('Stop'),
                          act,
                        );
                      else act();
                    },
                    r.state !== 'running',
                  )
                : '',
              edit,
              r.pending ? applyPending : '',
              r.kind === 'connection' ? test : '',
              menu,
            ]),
          ],
        );
      }, this);
      dom.content(
        this.connections,
        connections.length
          ? connections.map(node)
          : E('div', { class: 'veil-empty' }, [
              _(
                'Add a connection to choose a server and open local proxy listeners.',
              ),
            ]),
      );
      dom.content(
        this.relays,
        relays.length
          ? relays.map(node)
          : E(
              'p',
              { class: 'veil-muted' },
              _('No relays configured. Direct connections do not need one.'),
            ),
      );
      if (!this.writable) {
        this.connections
          .querySelectorAll('button:not([data-readonly]),input')
          .forEach((n) => {
            n.disabled = true;
          });
        this.relays
          .querySelectorAll('button:not([data-readonly]),input')
          .forEach((n) => {
            n.disabled = true;
          });
      }
    },
    edit: function (profile, revision, embeddedRelay) {
      var p = JSON.parse(JSON.stringify(profile));
      var cfg = p.config;
      var tls = cfg.tls || {};
      cfg.tls = tls;
      var hostPort = splitAddress(cfg.server);
      var name = input(p.name);
      name.id = 'veil-name';
      var host = input(hostPort[0]);
      var port = input(hostPort[1], 'number');
      port.min = 1;
      port.max = 65535;
      var secret = input(cfg.secret, 'password');
      var mode = select(
        [
          ['reality', 'REALITY'],
          ['tls', 'TLS 1.3'],
        ],
        tls.mode || 'reality',
      );
      var sni = input(tls.server_name);
      var key = input(tls.reality_public_key);
      var shortID = input(tls.short_id);
      var ca = input(tls.ca_file);
      var caPEM = E(
        'textarea',
        {
          class: 'cbi-input-textarea veil-editor',
          rows: 3,
          spellcheck: 'false',
        },
        tls.ca_pem || '',
      );
      var fingerprints = input(
        Array.isArray(tls.fingerprints)
          ? tls.fingerprints.join(', ')
          : tls.fingerprint,
      );
      var enabled = E('input', { type: 'checkbox', id: 'veil-enabled' });
      enabled.checked = p.enabled;
      var relay = select(
        [['', _('Direct connection')]].concat(
          this.rows
            .concat(
              embeddedRelay
                ? [
                    {
                      id: embeddedRelay.id,
                      kind: 'relay',
                      name: embeddedRelay.name,
                      server: embeddedRelay.config.server,
                    },
                  ]
                : [],
            )
            .filter(function (r) {
              return r.kind === 'relay';
            })
            .map(function (r) {
              return [r.id, r.name + ' · ' + r.server];
            }),
        ),
        p.relay_id || '',
      );
      var feedback = E('p', { class: 'veil-error', role: 'alert' });
      var fieldMap = {
        name: name,
        server: host,
        secret: secret,
        sni: sni,
        key: key,
      };
      var nextAvailable = bind(this.nextPort, this);
      var listeners = E('div', { class: 'veil-inlets' });
      var items = [];
      var addInlet = function (value) {
        var hp = splitAddress(value.listen || '127.0.0.1:1080');
        var kind = select(
          [
            ['mixed', 'SOCKS5 + HTTP'],
            ['socks', 'SOCKS5'],
            ['http', 'HTTP'],
          ],
          value.protocol || 'mixed',
        );
        var scope = select(
          [
            ['local', _('This device only')],
            ['lan', _('LAN address')],
            ['custom', _('Custom address')],
          ],
          hp[0] === '127.0.0.1' ? 'local' : 'custom',
        );
        var bind = input(hp[0]);
        var number = input(hp[1], 'number');
        number.min = 1;
        number.max = 65535;
        scope.addEventListener('change', function () {
          if (scope.value === 'local') bind.value = '127.0.0.1';
          if (scope.value === 'lan') {
            bind.value = '';
            bind.placeholder = '192.168.1.1';
            bind.focus();
          }
          bind.disabled = scope.value === 'local';
        });
        bind.disabled = scope.value === 'local';
        var entry = { kind: kind, bind: bind, port: number };
        items.push(entry);
        var row = E('div', { class: 'veil-inlet-row' }, [
          field(_('Protocol'), kind),
          field(_('Access'), scope),
          field(_('Listening IP'), bind),
          field(_('Port'), number),
          button(_('Remove'), function () {
            items.splice(items.indexOf(entry), 1);
            row.remove();
          }),
        ]);
        listeners.appendChild(row);
      };
      (p.inlets || []).forEach(addInlet);
      var realityFields = E('div', { class: 'veil-form-grid' }, [
        field(_('REALITY public key'), key),
        field(_('Short ID'), shortID),
        field(_('Browser templates (comma separated)'), fingerprints),
      ]);
      var caField = field(
        _('CA certificate path'),
        ca,
        _('Leave empty to use system certificate authorities.'),
      );
      var caPEMField = field(
        _('CA certificate PEM'),
        caPEM,
        _('Use a certificate path or pasted PEM, not both.'),
      );
      var transportUpdate = function () {
        realityFields.hidden = mode.value !== 'reality';
        caField.hidden = caPEMField.hidden = mode.value !== 'tls';
      };
      mode.addEventListener('change', transportUpdate);
      transportUpdate();
      var advanced = E(
        'textarea',
        {
          class: 'cbi-input-textarea veil-editor',
          rows: 8,
          spellcheck: 'false',
        },
        JSON.stringify(cfg, null, 2),
      );
      var parseAdvanced = function () {
        var config = JSON.parse(advanced.value);
        if (!config || typeof config !== 'object' || Array.isArray(config))
          throw new Error(_('Invalid JSON.'));
        return config;
      };
      var writeFields = function (changed) {
        var config = parseAdvanced();
        config.role = 'client';
        config.server = address(host.value.trim(), port.value);
        config.secret = secret.value.trim();
        config.tls = Object.assign({}, config.tls, {
          mode: mode.value,
          server_name: sni.value.trim(),
          reality_public_key: key.value.trim(),
          short_id: shortID.value.trim(),
          ca_file: ca.value.trim(),
          ca_pem: caPEM.value.trim(),
        });
        if (changed === ca && ca.value.trim()) {
          delete config.tls.ca_pem;
          caPEM.value = '';
        }
        if (changed === caPEM && caPEM.value.trim()) {
          delete config.tls.ca_file;
          ca.value = '';
        }
        // Preserve the imported single/list representation unless the user edits it.
        if (changed === fingerprints) {
          var names = fingerprints.value
            .split(',')
            .map(function (s) {
              return s.trim();
            })
            .filter(Boolean);
          delete config.tls.fingerprint;
          delete config.tls.fingerprints;
          if (names.length === 1) config.tls.fingerprint = names[0];
          if (names.length > 1) config.tls.fingerprints = names;
        }
        delete config.target;
        advanced.value = JSON.stringify(config, null, 2);
        transportUpdate();
        return config;
      };
      var configInputs = [
        host,
        port,
        secret,
        mode,
        sni,
        key,
        shortID,
        ca,
        caPEM,
        fingerprints,
      ];
      configInputs.forEach(function (node) {
        node.addEventListener('input', function () {
          try {
            writeFields(node);
            feedback.textContent = '';
          } catch (e) {
            feedback.textContent = e.message;
          }
        });
      });
      advanced.addEventListener('input', function () {
        try {
          var config = parseAdvanced(),
            transport = config.tls || {},
            hp = splitAddress(config.server);
          host.value = hp[0];
          port.value = hp[1];
          secret.value = config.secret || '';
          mode.value = transport.mode || 'tls';
          sni.value = transport.server_name || '';
          key.value = transport.reality_public_key || '';
          shortID.value = transport.short_id || '';
          ca.value = transport.ca_file || '';
          caPEM.value = transport.ca_pem || '';
          fingerprints.value =
            (Array.isArray(transport.fingerprints)
              ? transport.fingerprints.join(', ')
              : transport.fingerprint) || '';
          configInputs.forEach(function (node) {
            node.disabled = false;
          });
          feedback.textContent = '';
          transportUpdate();
        } catch (e) {
          configInputs.forEach(function (node) {
            node.disabled = true;
          });
          feedback.textContent = _('Invalid JSON.');
        }
      });
      var reveal = E('input', { type: 'checkbox' });
      reveal.addEventListener('change', function () {
        secret.type = reveal.checked ? 'text' : 'password';
      });
      var advancedNotice = E(
        'p',
        { class: 'veil-muted' },
        _(
          'Form fields and advanced JSON stay in sync. Advanced settings contain credentials.',
        ),
      );
      var form = E('div', { class: 'veil-editor-form' }, [
        feedback,
        E('div', { class: 'veil-form-grid' }, [
          field(_('Name'), name),
          field(_('Server address'), host),
          field(_('Server port'), port),
          field(_('Transport'), mode),
          field(_('Server name (SNI)'), sni),
          field(_('Authentication secret'), secret),
        ]),
        E('label', { class: 'veil-enable' }, [reveal, _('Show secret')]),
        realityFields,
        caField,
        caPEMField,
        p.kind === 'connection'
          ? E('div', {}, [
              E('h4', {}, _('Connection path')),
              field(_('Connect through'), relay),
              E('h4', {}, _('Local proxy listeners')),
              E(
                'p',
                { class: 'veil-muted' },
                _(
                  'SOCKS5 and HTTP may share a port or use separate listeners.',
                ),
              ),
              listeners,
              button(_('Add listener'), function () {
                var used = items.map(function (i) {
                  return Number(i.port.value);
                });
                var next = nextAvailable();
                while (used.indexOf(next) >= 0) next++;
                addInlet({ listen: '127.0.0.1:' + next });
              }),
              E('p', {}, [
                E('label', { class: 'veil-enable' }, [
                  enabled,
                  _('Start this connection when applied'),
                ]),
              ]),
            ])
          : '',
        E('details', {}, [
          E('summary', {}, _('Advanced JSON')),
          advancedNotice,
          advanced,
        ]),
      ]);
      var submit = bind(function (apply) {
        feedback.textContent = '';
        Object.keys(fieldMap).forEach(function (k) {
          fieldMap[k].removeAttribute('aria-invalid');
        });
        try {
          Object.keys(fieldMap).forEach(function (k) {
            if (
              (k !== 'key' || mode.value === 'reality') &&
              !fieldMap[k].value.trim()
            ) {
              fieldMap[k].setAttribute('aria-invalid', 'true');
              throw new Error(_('Fill in the highlighted field.'));
            }
          });
          var config = writeFields();
          p.config = config;
          p.name = name.value.trim();
          p.relay_id = p.kind === 'connection' ? relay.value : '';
          p.enabled = p.kind === 'connection' && enabled.checked;
          p.inlets =
            p.kind === 'connection'
              ? items.map(function (i) {
                  return {
                    protocol: i.kind.value,
                    listen: address(i.bind.value.trim(), i.port.value),
                  };
                })
              : [];
          var perform = bind(function () {
            saveButton.disabled = onlyButton.disabled = true;
            this.editorBusy = true;
            const controls = Array.from(
              form.querySelectorAll('input,select,textarea,button'),
            );
            const disabled = controls.map((n) => n.disabled);
            controls.forEach((n) => {
              n.disabled = true;
            });
            return save(
              p,
              revision,
              apply,
              embeddedRelay && p.relay_id === embeddedRelay.id
                ? embeddedRelay
                : null,
            )
              .then(function (r) {
                if (r.revision) {
                  revision = r.revision;
                  original = values();
                }
                return checked(r);
              })
              .then(
                bind(function () {
                  ui.hideModal();
                  return this.refresh(true);
                }, this),
              )
              .catch(function (e) {
                feedback.textContent = e.message;
                feedback.scrollIntoView({ block: 'nearest' });
              })
              .finally(() => {
                this.editorBusy = false;
                controls.forEach((n, i) => {
                  n.disabled = disabled[i];
                });
                saveButton.disabled = onlyButton.disabled = false;
              });
          }, this);
          // Keep the editor mounted so a failed apply can display its error.
          var disconnects =
            JSON.stringify(p.config) !== JSON.stringify(profile.config) ||
            p.relay_id !== (profile.relay_id || '') ||
            (profile.enabled && !p.enabled) ||
            (profile.inlets || []).some(function (old) {
              return !p.inlets.some(function (entry) {
                return entry.listen === old.listen;
              });
            });
          if (apply && revision && disconnects) {
            var current = this.rows.find(function (r) {
              return r.id === p.id;
            });
            if (
              current &&
              (current.state === 'running' || p.kind === 'relay')
            ) {
              confirmation.hidden = false;
              pending = perform;
              return;
            }
          }
          perform();
        } catch (e) {
          feedback.textContent = e.message;
        }
      }, this);
      var pending;
      var confirmation = E('div', { class: 'veil-confirm', hidden: true }, [
        E(
          'p',
          {},
          p.kind === 'relay'
            ? _('Applying relay changes may reconnect connections using it.')
            : _(
                'Applying connection changes may interrupt its active streams. Other connections keep running.',
              ),
        ),
        button(_('Cancel'), function () {
          confirmation.hidden = true;
        }),
        button(
          _('Apply changes'),
          function () {
            confirmation.hidden = true;
            if (pending) pending();
          },
          true,
        ),
      ]);
      var onlyButton = button(_('Save only'), function () {
        submit(false);
      });
      var saveButton = button(
        _('Save and apply'),
        function () {
          submit(true);
        },
        true,
      );
      const values = () =>
        JSON.stringify(
          Array.from(form.querySelectorAll('input,select,textarea'))
            .filter((n) => n !== reveal)
            .map((n) => (n.type === 'checkbox' ? n.checked : n.value)),
        );
      let original = values();
      const discard = E('div', { class: 'veil-confirm', hidden: true }, [
        E('p', {}, _('Discard unsaved changes?')),
      ]);
      discard.append(
        button(_('Keep editing'), () => {
          discard.hidden = true;
        }),
        button(_('Discard'), ui.hideModal),
      );
      const closeEditor = () => {
        if (this.editorBusy) return;
        if (values() === original) ui.hideModal();
        else {
          discard.hidden = false;
          discard.scrollIntoView({ block: 'nearest' });
        }
      };
      ui.showModal(
        p.kind === 'relay'
          ? revision
            ? _('Edit relay')
            : _('Add relay')
          : revision
            ? _('Edit connection')
            : _('Add connection'),
        [
          form,
          discard,
          confirmation,
          E('div', { class: 'veil-actions veil-right' }, [
            button(_('Cancel'), closeEditor),
            onlyButton,
            saveButton,
          ]),
        ],
      );
      for (const event of ['input', 'change'])
        form.addEventListener(event, () => {
          confirmation.hidden = true;
          pending = null;
        });
      this.cancelEditor = closeEditor;
      this.hasDraft = () => values() !== original;
      name.focus();
    },
    nextPort: function () {
      var used = this.rows.flatMap((r) =>
        (r.inlets || []).map((i) => Number(splitAddress(i.listen)[1])),
      );
      var port = 1080;
      while (used.includes(port)) port++;
      return port;
    },
    add: function (kind) {
      this.edit(
        {
          id:
            'node-' +
            Date.now().toString(36) +
            Math.random().toString(36).slice(2, 6),
          name: '',
          kind: kind,
          enabled: kind === 'connection',
          config: {
            role: 'client',
            tls: { mode: 'reality', record_padding: true },
          },
          inlets:
            kind === 'connection'
              ? [{ protocol: 'mixed', listen: '127.0.0.1:' + this.nextPort() }]
              : [],
        },
        '',
      );
    },
    importFile: function (file) {
      if (!file) return;
      if (file.size > 65536) {
        ui.addNotification(
          null,
          E('p', {}, _('Configuration must be smaller than 64 KiB.')),
          'error',
        );
        return;
      }
      return file
        .text()
        .then(
          bind(function (text) {
            var value = JSON.parse(text);
            if (!value || typeof value !== 'object' || Array.isArray(value))
              throw new Error(_('Configuration must be a JSON object.'));
            var embeddedRelay;
            if (value.profile) {
              if (value.version !== 1)
                throw new Error(_('Unsupported connection bundle version.'));
              embeddedRelay = value.relay;
              value = value.profile;
              if (embeddedRelay) {
                if (
                  embeddedRelay.kind !== 'relay' ||
                  value.relay_id !== embeddedRelay.id
                )
                  throw new Error(_('Invalid embedded relay.'));
                embeddedRelay.id =
                  'relay-' +
                  Date.now().toString(36) +
                  Math.random().toString(36).slice(2, 6);
                value.relay_id = embeddedRelay.id;
              }
            }
            var p;
            if (value.config && value.kind) {
              p = value;
              p.id = 'node-' + Date.now().toString(36);
              p.enabled = false;
              if (
                p.relay_id &&
                !embeddedRelay &&
                !this.rows.some(function (r) {
                  return r.id === p.relay_id && r.kind === 'relay';
                })
              )
                throw new Error(
                  _(
                    'Import the relay first, or choose a direct client configuration.',
                  ),
                );
            } else {
              if (value.role !== 'client' || value.target)
                throw new Error(_('Import a proxy client configuration.'));
              p = {
                id: 'node-' + Date.now().toString(36),
                name: value.server || '',
                kind: 'connection',
                enabled: false,
                config: value,
                inlets: [
                  {
                    protocol: value.inbound || 'socks',
                    listen: value.listen || '127.0.0.1:1080',
                  },
                ],
              };
            }
            this.edit(p, '', embeddedRelay);
          }, this),
        )
        .catch(function (e) {
          ui.addNotification(null, E('p', {}, e.message), 'error');
        });
    },
    render: function (data) {
      this.rows = data.connections || [];
      this.notice = E('p', { class: 'veil-error', role: 'alert' });
      this.connections = E('div', { class: 'veil-cards' });
      this.relays = E('div', { class: 'veil-cards' });
      var file = E('input', {
        type: 'file',
        accept: '.json,application/json',
        hidden: true,
        id: 'veil-import',
        change: bind(function (e) {
          this.importFile(e.target.files[0]);
          file.value = '';
        }, this),
      });
      var add = button(
        _('Add connection'),
        bind(function () {
          this.add('connection');
        }, this),
        true,
      );
      var relay = button(
        _('Add relay'),
        bind(function () {
          this.add('relay');
        }, this),
      );
      var imp = button(_('Import'), async () => {
        if (options.importFile) {
          try {
            const text = await options.importFile();
            if (text) this.importFile(new File([text], 'connection.json'));
          } catch (e) {
            ui.addNotification(null, E('p', {}, e.message));
          }
        } else file.click();
      });
      var paste = button(_('Paste JSON'), () => {
        const text = E('textarea', {
          rows: 12,
          class: 'veil-editor',
          spellcheck: false,
          'aria-label': _('Connection JSON'),
        });
        ui.showModal(_('Import connection'), [
          E('p', { role: 'alert', class: 'veil-error' }),
          text,
          E('div', { class: 'veil-actions veil-right' }, [
            button(_('Cancel'), () => this.cancelEditor?.()),
            button(
              _('Import'),
              () => this.importFile(new File([text.value], 'connection.json')),
              true,
            ),
          ]),
        ]);
        const discard = E('div', { class: 'veil-confirm', hidden: true }, [
          E('p', {}, _('Discard unsaved changes?')),
          button(_('Keep editing'), () => {
            discard.hidden = true;
          }),
          button(_('Discard'), ui.hideModal),
        ]);
        dialog.append(discard);
        const cancel = () => {
          if (!text.value.trim()) ui.hideModal();
          else discard.hidden = false;
        };
        this.cancelEditor = cancel;
        this.hasDraft = () => !!text.value.trim();
        text.focus();
      });
      add.disabled =
        relay.disabled =
        imp.disabled =
        paste.disabled =
          !this.writable;
      var root = E(
        'div',
        { class: 'veil-page', 'data-theme': options.theme?.() || '' },
        [
          options.cssURL
            ? E('link', { rel: 'stylesheet', href: options.cssURL })
            : '',
          E('div', { class: 'veil-head' }, [
            E('div', {}, [
              E('h2', {}, _('Connections')),
              E(
                'p',
                { class: 'veil-muted' },
                _('Servers, connection paths and local proxy listeners.'),
              ),
            ]),
            E('div', { class: 'veil-actions' }, [imp, paste, add, file]),
          ]),
          this.notice,
          this.connections,
          E('div', { class: 'veil-head veil-relay-heading' }, [
            E('h3', {}, _('Relays')),
            relay,
          ]),
          this.relays,
          E(
            'p',
            { class: 'veil-muted veil-footnote' },
            _(
              'Test Google sends an HTTPS request through the selected connection. Time includes any required tunnel setup and TLS handshake; it is not ping RTT.',
            ),
          ),
        ],
      );
      this.draw();
      this.timer = setInterval(() => {
        if (!document.hidden && !this.editorOpen) this.refresh();
      }, 5000);
      return root;
    },
    handleSave: null,
    handleSaveApply: null,
    handleReset: null,
  };
  view.hasDraft = () => false;
  view.setLanguage = (language) => {
    if (view.editorOpen) return false;
    options.language = language;
    clearInterval(view.timer);
    options.root.replaceChildren(view.render({ connections: view.rows }));
    return true;
  };
  window.addEventListener('beforeunload', (event) => {
    if (view.hasDraft()) {
      event.preventDefault();
      event.returnValue = '';
    }
  });
  const load = () =>
    view
      .load()
      .then((data) => {
        options.root.replaceChildren(view.render(data));
      })
      .catch((error) => {
        options.root.replaceChildren(
          E('p', { role: 'alert' }, error.message),
          button(_('Retry'), load),
        );
      });
  load();
  return view;
};
