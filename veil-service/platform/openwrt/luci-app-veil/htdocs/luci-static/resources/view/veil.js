'use strict';
'require view';
'require rpc';

const fields = {
  connections: [],
  connection_get: ['id'],
  connection_export: ['id'],
  connection_test: ['id'],
  connection_save: ['profile', 'expected_revision', 'apply', 'relay'],
  connection_start: ['id', 'expected_revision'],
  connection_stop: ['id', 'expected_revision'],
  connection_delete: ['id', 'expected_revision'],
};
const methods = Object.fromEntries(
  Object.entries(fields).map(([method, params]) => [
    method,
    rpc.declare({ object: 'veil', method, params }),
  ]),
);
const asset = (name) => L.resource('veil/' + name) + '?v=__VEIL_UI_VERSION__';
function script(name) {
  return new Promise((resolve, reject) => {
    const node = E('script', { src: asset(name) });
    node.onload = resolve;
    node.onerror = () => reject(new Error('Veil UI could not be loaded'));
    document.head.appendChild(node);
  });
}
return view.extend({
  load: function () {
    return window.VeilConnections
      ? Promise.resolve()
      : script('i18n.js').then(() => script('connections.js'));
  },
  render: function () {
    const root = E('div');
    window.VeilConnections({
      root,
      writable: L.hasViewPermission(),
      language: document.documentElement.lang.toLowerCase().startsWith('zh')
        ? 'zh'
        : 'en',
      cssURL: asset('connections.css'),
      theme: () => {
        const rgb = getComputedStyle(document.body).backgroundColor.match(
          /\d+(?:\.\d+)?/g,
        );
        return rgb && 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2] < 128
          ? 'dark'
          : 'light';
      },
      request: (q) =>
        methods[q.action](...fields[q.action].map((key) => q[key])),
    });
    return root;
  },
  handleSave: null,
  handleSaveApply: null,
  handleReset: null,
});
