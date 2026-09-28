'use strict';
'require view';
'require dom';
'require rpc';
'require ui';
'require poll';

var list = rpc.declare({ object: 'veil', method: 'connections' });
var exportConnection = rpc.declare({ object: 'veil', method: 'connection_export', params: ['id'] });
var get = rpc.declare({ object: 'veil', method: 'connection_get', params: ['id'] });
var save = rpc.declare({
	object: 'veil',
	method: 'connection_save',
	params: ['profile', 'expected_revision', 'apply', 'relay'],
});
var start = rpc.declare({
	object: 'veil',
	method: 'connection_start',
	params: ['id', 'expected_revision'],
});
var stop = rpc.declare({
	object: 'veil',
	method: 'connection_stop',
	params: ['id', 'expected_revision'],
});
var remove = rpc.declare({
	object: 'veil',
	method: 'connection_delete',
	params: ['id', 'expected_revision'],
});
var probe = rpc.declare({ object: 'veil', method: 'connection_test', params: ['id'] });

function checked(r) {
	if (r.error) throw new Error(r.error.message);
	return r;
}
function button(label, action, primary) {
	return E(
		'button',
		{
			type: 'button',
			class: 'cbi-button ' + (primary ? 'cbi-button-positive' : 'cbi-button-neutral'),
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

return view.extend({
	load: function () {
		this.writable = L.hasViewPermission();
		return list().then(checked);
	},
	refresh: function () {
		return list()
			.then(checked)
			.then(
				L.bind(function (r) {
					this.rows = r.connections || [];
					this.draw();
				}, this),
			)
			.catch(
				L.bind(function (e) {
					this.notice.textContent = e.message;
				}, this),
			);
	},
	action: function (operation) {
		return Promise.resolve()
			.then(operation)
			.then(checked)
			.catch(function (e) {
				ui.addNotification(null, E('p', {}, e.message), 'error');
			})
			.finally(L.bind(this.refresh, this));
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
	draw: function () {
		this.notice.textContent = '';
		var connections = this.rows.filter(function (r) {
			return r.kind === 'connection';
		});
		var relays = this.rows.filter(function (r) {
			return r.kind === 'relay';
		});
		var node = L.bind(function (r) {
			var enabled = E('input', { type: 'checkbox', class: 'veil-switch' });
			enabled.checked = r.enabled;
			enabled.disabled = !this.writable;
			enabled.addEventListener(
				'change',
				L.bind(function () {
					enabled.checked = r.enabled;
					var run = L.bind(function () {
						return this.action(function () {
							return (r.enabled ? stop : start)(r.id, r.revision);
						});
					}, this);
					if (r.enabled)
						this.confirm(
							_('Stop connection?'),
							_('Only this connection and its proxy listeners will stop.'),
							_('Stop'),
							run,
						);
					else run();
				}, this),
			);
			var status =
				r.kind === 'relay'
					? _('Relay profile')
					: r.state === 'running'
						? _('Proxy started')
						: r.enabled
							? _('Start failed')
							: _('Connection disabled');
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
				L.bind(function () {
					this.testing = this.testing || {};
					this.testing[r.id] = true;
					this.draw();
					test.disabled = true;
					test.textContent = _('Testing…');
					this.action(function () {
						return probe(r.id);
					}).finally(
						L.bind(function () {
							delete this.testing[r.id];
							this.draw();
						}, this),
					);
				}, this),
			);
			test.disabled = !this.writable || testing;
			var edit = button(
				_('Edit'),
				L.bind(function () {
					this.action(
						L.bind(function () {
							return get(r.id)
								.then(checked)
								.then(
									L.bind(function (v) {
										this.edit(v.profile, r.revision);
										return v;
									}, this),
								);
						}, this),
					);
				}, this),
			);
			edit.disabled = !this.writable;
			var menu = E('details', { class: 'veil-more' }, [E('summary', {}, _('More'))]);
			var exportButton = button(_('Export'), function () {
				exportConnection(r.id)
					.then(checked)
					.then(function (v) {
						var url = URL.createObjectURL(
							new Blob(
								[JSON.stringify({ version: 1, profile: v.profile, relay: v.relay }, null, 2)],
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
				L.bind(function () {
					this.action(
						L.bind(function () {
							return get(r.id)
								.then(checked)
								.then(
									L.bind(function (v) {
										v.profile.id =
											'node-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
										v.profile.name += ' (' + _('Copy') + ')';
										v.profile.enabled = false;
										v.profile.inlets = [];
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
				L.bind(function () {
					this.confirm(
						_('Delete connection?'),
						_('Delete this profile and stop its listeners? Relays in use cannot be deleted.'),
						_('Delete'),
						L.bind(function () {
							this.action(function () {
								return remove(r.id, r.revision);
							});
						}, this),
					);
				}, this),
			);
			del.disabled = !this.writable;
			menu.appendChild(E('div', { class: 'veil-actions' }, [exportButton, duplicate, del]));
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
						i.protocol === 'mixed' ? 'SOCKS5 + HTTP' : i.protocol === 'socks' ? 'SOCKS5' : 'HTTP',
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
						E('div', {}, [E('h3', {}, r.name), E('code', { class: 'veil-server' }, r.server)]),
						E(
							'span',
							{ class: 'veil-badge ' + (r.state === 'running' ? 'veil-running' : '') },
							status,
						),
					]),
					r.kind === 'connection' ? route : '',
					r.kind === 'connection'
						? E(
								'div',
								{ class: 'veil-endpoints' },
								endpoints.length ? endpoints : [E('span', {}, _('No proxy listeners'))],
							)
						: E(
								'p',
								{ class: 'veil-muted' },
								_(
									'Shared relay settings. Test Google on a complete connection that uses this relay.',
								),
							),
					r.pending
						? E('p', { class: 'veil-notice' }, _('Saved changes are waiting to be applied.'))
						: '',
					r.error || r.last_connection_error
						? E('p', { class: 'veil-error' }, r.error || r.last_connection_error)
						: '',
					r.kind === 'connection'
						? E('div', { class: 'veil-test-result' }, [
								E('span', {}, _('Google HTTPS') + ': '),
								E('strong', {}, testText),
								r.probe ? E('small', {}, new Date(r.probe.at).toLocaleTimeString()) : '',
								r.probe && r.probe.error ? E('p', { class: 'veil-error' }, r.probe.error) : '',
							])
						: '',
					E('div', { class: 'veil-actions' }, [
						r.kind === 'connection'
							? E('label', { class: 'veil-enable' }, [
									enabled,
									E('span', {}, _('Enable connection')),
								])
							: '',
						edit,
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
						_('Add a connection to choose a server and open local proxy listeners.'),
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
		var fieldMap = { name: name, server: host, secret: secret, sni: sni, key: key };
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
					['local', _('This router only')],
					['lan', _('LAN')],
					['custom', _('Custom address')],
				],
				hp[0] === '127.0.0.1' ? 'local' : hp[0] === window.location.hostname ? 'lan' : 'custom',
			);
			var bind = input(hp[0]);
			var number = input(hp[1], 'number');
			number.min = 1;
			number.max = 65535;
			scope.addEventListener('change', function () {
				if (scope.value === 'local') bind.value = '127.0.0.1';
				if (scope.value === 'lan') bind.value = window.location.hostname;
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
		]);
		var caField = field(
			_('CA certificate path'),
			ca,
			_('Leave empty to use system certificate authorities.'),
		);
		var transportUpdate = function () {
			realityFields.hidden = mode.value !== 'reality';
			caField.hidden = mode.value !== 'tls';
		};
		mode.addEventListener('change', transportUpdate);
		transportUpdate();
		var advanced = E(
			'textarea',
			{ class: 'cbi-input-textarea veil-editor', rows: 8, spellcheck: 'false' },
			JSON.stringify(cfg, null, 2),
		);
		var reveal = E('input', { type: 'checkbox' });
		reveal.addEventListener('change', function () {
			secret.type = reveal.checked ? 'text' : 'password';
		});
		var advancedNotice = E(
			'p',
			{ class: 'veil-muted' },
			_('Advanced settings contain credentials. Form fields override matching JSON values.'),
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
			p.kind === 'connection'
				? E('div', {}, [
						E('h4', {}, _('Connection path')),
						field(_('Connect through'), relay),
						E('h4', {}, _('Local proxy listeners')),
						E(
							'p',
							{ class: 'veil-muted' },
							_('SOCKS5 and HTTP may share a port or use separate listeners.'),
						),
						listeners,
						button(_('Add listener'), function () {
							addInlet({});
						}),
						E('p', {}, [
							E('label', { class: 'veil-enable' }, [enabled, _('Enable this connection')]),
						]),
					])
				: '',
			E('details', {}, [E('summary', {}, _('Advanced JSON')), advancedNotice, advanced]),
		]);
		var submit = L.bind(function (apply) {
			feedback.textContent = '';
			Object.keys(fieldMap).forEach(function (k) {
				fieldMap[k].removeAttribute('aria-invalid');
			});
			try {
				Object.keys(fieldMap).forEach(function (k) {
					if ((k !== 'key' || mode.value === 'reality') && !fieldMap[k].value.trim()) {
						fieldMap[k].setAttribute('aria-invalid', 'true');
						throw new Error(_('Fill in the highlighted field.'));
					}
				});
				var config = JSON.parse(advanced.value);
				if (!config || typeof config !== 'object' || Array.isArray(config))
					throw new Error(_('Invalid JSON.'));
				config.role = 'client';
				config.server = address(host.value.trim(), port.value);
				config.secret = secret.value.trim();
				config.tls = Object.assign({}, config.tls, {
					mode: mode.value,
					server_name: sni.value.trim(),
					reality_public_key: key.value.trim(),
					short_id: shortID.value.trim(),
					ca_file: ca.value.trim(),
				});
				delete config.target;
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
				var perform = L.bind(function () {
					saveButton.disabled = onlyButton.disabled = true;
					return save(
						p,
						revision,
						apply,
						embeddedRelay && p.relay_id === embeddedRelay.id ? embeddedRelay : null,
					)
						.then(function (r) {
							if (r.revision) revision = r.revision;
							return checked(r);
						})
						.then(
							L.bind(function () {
								ui.hideModal();
								return this.refresh();
							}, this),
						)
						.catch(function (e) {
							feedback.textContent = e.message;
							feedback.scrollIntoView({ block: 'nearest' });
						})
						.finally(function () {
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
					if (current && (current.state === 'running' || p.kind === 'relay')) {
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
		ui.showModal(revision ? _('Edit connection') : _('Add connection'), [
			form,
			confirmation,
			E('div', { class: 'veil-actions veil-right' }, [
				button(_('Cancel'), ui.hideModal),
				onlyButton,
				saveButton,
			]),
		]);
	},
	add: function (kind) {
		this.edit(
			{
				id: 'node-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6),
				name: '',
				kind: kind,
				enabled: kind === 'connection',
				config: { role: 'client', tls: { mode: 'reality', record_padding: true } },
				inlets: kind === 'connection' ? [{ protocol: 'mixed', listen: '127.0.0.1:1080' }] : [],
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
		file
			.text()
			.then(
				L.bind(function (text) {
					var value = JSON.parse(text);
					var embeddedRelay;
					if (value.profile) {
						if (value.version !== 1) throw new Error(_('Unsupported connection bundle version.'));
						embeddedRelay = value.relay;
						value = value.profile;
						if (embeddedRelay) {
							if (embeddedRelay.kind !== 'relay' || value.relay_id !== embeddedRelay.id)
								throw new Error(_('Invalid embedded relay.'));
							embeddedRelay.id =
								'relay-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
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
								_('Import the relay first, or choose a direct client configuration.'),
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
								{ protocol: value.inbound || 'socks', listen: value.listen || '127.0.0.1:1080' },
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
			change: L.bind(function (e) {
				this.importFile(e.target.files[0]);
				file.value = '';
			}, this),
		});
		var add = button(
			_('Add connection'),
			L.bind(function () {
				this.add('connection');
			}, this),
			true,
		);
		var relay = button(
			_('Add relay'),
			L.bind(function () {
				this.add('relay');
			}, this),
		);
		var imp = button(_('Import'), function () {
			file.click();
		});
		add.disabled = relay.disabled = imp.disabled = !this.writable;
		var root = E('div', { class: 'veil-page' }, [
			E('link', { rel: 'stylesheet', href: L.resource('veil.css') }),
			E('div', { class: 'veil-head' }, [
				E('div', {}, [
					E('h2', {}, 'Veil'),
					E(
						'p',
						{ class: 'veil-muted' },
						_('Servers, connection paths and local proxy listeners.'),
					),
				]),
				E('div', { class: 'veil-actions' }, [imp, add, file]),
			]),
			this.notice,
			this.connections,
			E('div', { class: 'veil-head veil-relay-heading' }, [E('h3', {}, _('Relays')), relay]),
			this.relays,
			E(
				'p',
				{ class: 'veil-muted veil-footnote' },
				_(
					'Test Google sends an HTTPS request through the selected connection. Time includes any required tunnel setup and TLS handshake; it is not ping RTT.',
				),
			),
		]);
		this.draw();
		poll.add(L.bind(this.refresh, this), 5);
		return root;
	},
	handleSave: null,
	handleSaveApply: null,
	handleReset: null,
});
