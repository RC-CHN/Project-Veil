'use strict';
'require view';
'require rpc';
'require ui';
'require poll';

var status = rpc.declare({ object: 'veil', method: 'status' });
var configuration = rpc.declare({ object: 'veil', method: 'config' });
var validate = rpc.declare({ object: 'veil', method: 'validate', params: ['config'] });
var save = rpc.declare({ object: 'veil', method: 'save', params: ['config', 'expected_revision'] });
var start = rpc.declare({ object: 'veil', method: 'start', params: ['expected_revision'] });
var stop = rpc.declare({ object: 'veil', method: 'stop' });
var restart = rpc.declare({ object: 'veil', method: 'restart', params: ['expected_revision'] });

function checked(result) {
	if (result.error) {
		var messages = {
			invalid_config: _('Configuration validation failed.'),
			conflict: _('The saved configuration changed elsewhere. Reload it before saving again.'),
			no_config: _('Import and save a configuration first.'),
			start_failed: _('The proxy could not start. Check the listening address and certificate paths.'),
			save_failed: _('The configuration could not be saved. Check available storage and permissions.'),
			unavailable: _('The control service is unavailable.'),
			durability_uncertain: _('Configuration saved, but disk durability could not be confirmed. Check the storage device.')
		};
		throw new Error((messages[result.error.code] || _('Operation failed.')) + ' ' + result.error.message);
	}
	return result;
}

return view.extend({
	load: function() {
		this.writable = L.hasViewPermission();
		return (this.writable ? configuration() : status()).then(checked).catch(function(error) {
			return { unavailable: error.message };
		});
	},

	parse: function() {
		var text = this.editor.value;
		if (new Blob([text]).size > 65536) throw new Error(_('Configuration must be smaller than 64 KiB.'));
		var cfg;
		try { cfg = JSON.parse(text); }
		catch (_) { throw new Error(_('Invalid JSON. Check the configuration before saving.')); }
		if (!cfg || typeof cfg !== 'object' || Array.isArray(cfg)) throw new Error(_('The configuration must be a JSON object.'));
		if (cfg.role !== 'client' || cfg.target) throw new Error(_('Import a proxy client configuration without a fixed forwarding target.'));
		return cfg;
	},

	syncFields: function() {
		try {
			var cfg = JSON.parse(this.editor.value);
			this.protocol.value = cfg.inbound || 'socks';
			this.address.value = cfg.listen || '127.0.0.1:1080';
		} catch (_) { /* Keep the draft visible while JSON is being edited. */ }
	},

	update: function(result) {
		this.current = result.status || {};
		var s = this.current;
		this.unavailable = result.unavailable || '';
		this.badge.textContent = this.unavailable ? _('Service unavailable') : s.state === 'running' ? _('Running') : _('Stopped');
		this.badge.className = 'veil-badge ' + (this.unavailable ? 'veil-error' : s.state === 'running' ? 'veil-running' : '');
		this.listen.textContent = s.listen || '—';
		this.role.textContent = s.role === 'client' ? _('Client') : s.role === 'server' ? _('Server') : '—';
		this.count.textContent = String((s.stats || {}).completed || 0);
		this.notice.textContent = this.unavailable ? _('Unable to reach Veil. Check that the service is started, then refresh.') :
			s.restart_required ? _('A saved configuration is waiting to be applied. Applying it will disconnect current connections.') :
			!s.saved_revision ? _('Import a configuration to get started.') : '';
		this.failure.textContent = s.error || s.last_connection_error || '';
		this.buttons.start.disabled = this.busy || !this.writable || !!this.unavailable || s.state === 'running' || !s.saved_revision;
		this.buttons.stop.disabled = this.busy || !this.writable || !!this.unavailable || s.state !== 'running';
		this.buttons.apply.disabled = this.busy || !this.writable || !!this.unavailable || !s.restart_required;
		this.buttons.save.disabled = this.busy || !this.writable || !!this.unavailable || this.editor.value === this.savedText;
		this.buttons.validate.disabled = this.busy || !this.writable || !!this.unavailable || !this.editor.value.trim();
		this.buttons.reload.disabled = this.busy || !this.writable;
		this.file.disabled = this.busy || !this.writable;
		this.buttons.choose.disabled = this.file.disabled;
		this.editor.disabled = this.busy || !this.writable;
		this.protocol.disabled = this.address.disabled = this.busy || !this.writable || !this.editor.value.trim();
	},

	run: function(operation) {
		this.busy = true;
		this.feedback.textContent = '';
		this.update({ status: this.current, unavailable: this.unavailable });
		return Promise.resolve().then(operation).catch(function(error) {
			ui.addNotification(null, E('p', {}, error.message), 'error');
		}).finally(L.bind(function() {
			this.busy = false;
			return this.refresh();
		}, this));
	},

	refresh: function() {
		return status().then(checked).then(L.bind(this.update, this)).catch(L.bind(function(error) {
			this.update({ unavailable: error.message });
		}, this));
	},

	confirmApply: function() {
		var revision = this.current.saved_revision;
		ui.showModal(_('Apply saved configuration?'), [
			E('p', {}, _('Current connections will be disconnected. Unsaved edits in the editor are not applied.')),
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'cbi-button', click: ui.hideModal }, _('Cancel')),
				E('button', { 'class': 'cbi-button cbi-button-positive', click: L.bind(function() {
					ui.hideModal();
					return this.run(L.bind(function() { return restart(revision).then(checked); }, this));
				}, this) }, _('Apply and restart'))
			])
		]);
	},

	render: function(initial) {
		this.current = initial.status || {};
		this.revision = this.current.saved_revision || '';
		this.savedText = initial.config ? JSON.stringify(initial.config, null, 2) : '';
		this.buttons = {};
		var button = L.bind(function(name, label, action, primary) {
			var b = E('button', { 'type': 'button', 'class': 'cbi-button ' + (primary ? 'cbi-button-positive' : 'cbi-button-neutral'), click: action }, label);
			this.buttons[name] = b;
			return b;
		}, this);
		this.badge = E('span', { 'class': 'veil-badge', 'role': 'status', 'aria-live': 'polite' });
		this.listen = E('strong');
		this.role = E('strong');
		this.count = E('strong');
		this.notice = E('p', { 'class': 'veil-notice', 'role': 'status' });
		this.failure = E('p', { 'class': 'veil-failure' });
		this.feedback = E('p', { 'class': 'veil-feedback', 'role': 'status', 'aria-live': 'polite' });
		this.editor = E('textarea', {
			'id': 'veil-json', 'class': 'cbi-input-textarea veil-editor', 'rows': 15,
			'spellcheck': 'false', 'autocomplete': 'off',
			input: L.bind(function() { this.syncFields(); this.update({ status: this.current, unavailable: this.unavailable }); }, this)
		}, this.savedText);
		var editListener = L.bind(function() {
			try {
				var cfg = this.parse();
				cfg.inbound = this.protocol.value;
				cfg.listen = this.address.value;
				this.editor.value = JSON.stringify(cfg, null, 2);
				this.update({ status: this.current, unavailable: this.unavailable });
			} catch (error) { ui.addNotification(null, E('p', {}, error.message), 'error'); }
		}, this);
		this.protocol = E('select', { id: 'veil-protocol', 'class': 'cbi-input-select', change: editListener }, [
			E('option', { value: 'mixed' }, 'SOCKS5 + HTTP'), E('option', { value: 'socks' }, 'SOCKS5'), E('option', { value: 'http' }, 'HTTP')
		]);
		this.address = E('input', { id: 'veil-address', 'class': 'cbi-input-text', type: 'text', placeholder: '127.0.0.1:1080', change: editListener });
		this.syncFields();
		this.fileName = E('span', { 'class': 'veil-filename' }, _('No file selected'));
		this.file = E('input', { 'id': 'veil-file', 'type': 'file', 'hidden': true, 'accept': '.json,application/json', change: L.bind(function(ev) {
			var file = ev.target.files[0];
			if (!file) return;
			this.fileName.textContent = file.name;
			this.run(L.bind(function() {
				if (file.size > 65536) throw new Error(_('Configuration must be smaller than 64 KiB.'));
				return file.text().then(L.bind(function(text) {
					this.editor.value = text;
					this.parse();
					this.syncFields();
					return validate(this.parse()).then(checked);
				}, this)).then(L.bind(function() { this.feedback.textContent = _('Configuration imported and validated. Save it when ready.'); }, this));
			}, this));
		}, this) });
		var reload = L.bind(function() {
			if (this.editor.value !== this.savedText && !window.confirm(_('Discard unsaved edits and reload the saved configuration?'))) return;
			return this.run(L.bind(function() {
				return configuration().then(checked).then(L.bind(function(r) {
					this.revision = r.status.saved_revision || '';
					this.savedText = r.config ? JSON.stringify(r.config, null, 2) : '';
					this.editor.value = this.savedText;
					this.syncFields();
				}, this));
			}, this));
		}, this);
		var root = E('div', { 'class': 'veil-page' }, [
			E('link', { rel: 'stylesheet', href: L.resource('veil.css') }),
			E('div', { 'class': 'veil-head' }, [E('div', {}, [E('h2', {}, 'Veil'), E('p', { 'class': 'veil-help' }, _('Manage your proxy connection and configuration.'))]), this.badge]),
			E('section', { 'class': 'cbi-section veil-panel' }, [
				E('h3', {}, _('Connection')),
				E('div', { 'class': 'veil-metrics' }, [
					E('div', { 'class': 'veil-metric' }, [E('span', {}, _('Mode')), this.role]),
					E('div', { 'class': 'veil-metric' }, [E('span', {}, _('Listening address')), this.listen]),
					E('div', { 'class': 'veil-metric' }, [E('span', {}, _('Completed connections')), this.count])
				]), this.notice, this.failure,
				E('div', { 'class': 'veil-actions' }, [
					button('start', _('Start proxy'), L.bind(function() { return this.run(L.bind(function() { return start(this.current.saved_revision).then(checked); }, this)); }, this), true),
					button('stop', _('Stop proxy'), L.bind(function() { return this.run(function() { return stop().then(checked); }); }, this)),
					button('apply', _('Apply saved configuration'), L.bind(this.confirmApply, this)),
					button('refresh', _('Refresh'), L.bind(this.refresh, this))
				])
			]),
			E('section', { 'class': 'cbi-section veil-panel' }, [
				E('h3', {}, _('Configuration')),
				E('p', { 'class': 'veil-help' }, _('Import the JSON configuration provided by your server administrator. Saving keeps current connections running; apply saved changes separately.')),
				E('div', { 'class': 'veil-file' }, [E('span', {}, _('Choose configuration file')),
					E('div', { 'class': 'veil-file-row' }, [button('choose', _('Choose file'), L.bind(function() { this.file.click(); }, this)), this.fileName]), this.file]),
				E('div', { 'class': 'veil-listener' }, [
					E('div', {}, [E('label', { 'for': 'veil-protocol' }, _('Proxy protocol')), this.protocol]),
					E('div', {}, [E('label', { 'for': 'veil-address' }, _('Listening address')), this.address])
				]),
				E('p', { 'class': 'veil-help' }, _('Use 127.0.0.1 for this router only, or a LAN address for other devices. HTTP supports CONNECT for HTTPS.')),
				this.feedback,
				E('details', {}, [E('summary', {}, _('Advanced: edit configuration JSON')), E('label', { 'for': 'veil-json', 'class': 'veil-help' }, _('Contains credentials. Share only with trusted administrators.')), this.editor]),
				E('div', { 'class': 'veil-actions' }, [
					button('save', _('Save configuration'), L.bind(function() {
						return this.run(L.bind(function() {
							return save(this.parse(), this.revision).then(checked).then(L.bind(function(r) {
								this.revision = r.revision;
								this.savedText = this.editor.value;
								this.feedback.textContent = _('Configuration saved.');
							}, this));
						}, this));
					}, this), true),
					button('validate', _('Validate'), L.bind(function() { return this.run(L.bind(function() { return validate(this.parse()).then(checked).then(L.bind(function() { this.feedback.textContent = _('Configuration is valid.'); }, this)); }, this)); }, this)),
					button('reload', _('Reload saved configuration'), reload)
				])
			])
		]);
		this.buttons.reload.disabled = !this.writable;
		this.update(initial);
		poll.add(L.bind(this.refresh, this), 5);
		return root;
	},
	handleSave: null,
	handleSaveApply: null,
	handleReset: null
});
