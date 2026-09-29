<div id="veil-page">
  <div class="veil-heading"><h1>Veil</h1></div>
  <div id="veil-connections"></div>
  <details id="veil-standalone" class="content-box veil-standalone">
    <summary data-i18n="Advanced: standalone server or fixed forwarding"></summary>
    <p class="text-muted" data-i18n="Use this separate instance to run a Veil server or forward to a fixed target. Manage proxy client connections in the list above."></p>
    <p id="veil-notice" class="alert alert-info" role="status" hidden></p>
    <p id="veil-error" class="alert alert-danger" role="alert" hidden></p>
    <p id="veil-feedback" class="alert alert-success" role="status" hidden></p>
    <p><strong id="veil-state"></strong> <code id="veil-listen"></code></p>
    <div class="veil-actions">
      <button id="veil-start" class="btn btn-primary" data-i18n="Start" disabled></button>
      <button id="veil-stop" class="btn btn-default" data-i18n="Stop" disabled></button>
      <button id="veil-migrate" class="btn btn-default" data-i18n="Move client to connection list" hidden></button>
      <button id="veil-refresh" class="btn btn-default" data-i18n="Refresh"></button>
    </div>
    <fieldset id="veil-settings" hidden>
      <div class="veil-actions">
        <button id="veil-choose" class="btn btn-default" data-i18n="Import"></button>
        <button id="veil-export" class="btn btn-default" data-i18n="Export"></button>
        <button id="veil-reload" class="btn btn-default" data-i18n="Discard edits"></button>
        <input id="veil-file" type="file" accept=".json,application/json" hidden>
      </div>
      <label for="veil-json" class="text-muted" data-i18n="Configuration JSON (contains credentials)"></label>
      <textarea id="veil-json" class="form-control" rows="12" spellcheck="false" autocomplete="off"></textarea>
      <label class="veil-boot"><input id="veil-boot-enabled" type="checkbox"> <span data-i18n="Start this instance when the system boots"></span></label>
      <div class="veil-actions">
        <button id="veil-save-apply" class="btn btn-primary" data-i18n="Save and apply" disabled></button>
        <button id="veil-save" class="btn btn-default" data-i18n="Save only" disabled></button>
        <button id="veil-validate" class="btn btn-default" data-i18n="Validate" disabled></button>
      </div>
    </fieldset>
    <details><summary data-i18n="Diagnostics"></summary><pre id="veil-status-json"></pre></details>
  </details>
</div>
<link rel="stylesheet" href="/ui/js/veil/shared/connections.css?v=__VEIL_UI_VERSION__">
<script>window.veilUI = {language: {{ veilLanguage }}, writable: {{ veilWritable }}};</script>
<script src="/ui/js/veil/shared/i18n.js?v=__VEIL_UI_VERSION__"></script>
<script src="/ui/js/veil/shared/connections.js?v=__VEIL_UI_VERSION__"></script>
<script src="/ui/js/veil/page.js?v=__VEIL_UI_VERSION__"></script>
