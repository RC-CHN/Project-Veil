<div id="veil-page">
  <div class="veil-heading">
    <div><h1>Veil</h1><p class="text-muted" data-i18n="Manage your proxy connection and configuration."></p></div>
    <span id="veil-state" class="label label-default" role="status"></span>
  </div>
  <section class="content-box veil-card">
    <h2 data-i18n="Connection"></h2>
    <div class="veil-metrics">
      <div><span data-i18n="Mode"></span><strong id="veil-role">—</strong></div>
      <div><span data-i18n="Listening address"></span><strong id="veil-listen">—</strong></div>
      <div><span data-i18n="Completed connections"></span><strong id="veil-count">0</strong></div>
    </div>
    <p id="veil-notice" class="alert alert-info" role="status" hidden></p>
    <p id="veil-error" class="alert alert-danger" role="alert" hidden></p>
    <div class="veil-actions">
      <button id="veil-start" class="btn btn-primary" data-i18n="Start proxy" disabled></button>
      <button id="veil-stop" class="btn btn-default" data-i18n="Stop proxy" disabled></button>
      <button id="veil-apply" class="btn btn-default" data-i18n="Apply saved configuration" disabled></button>
      <button id="veil-refresh" class="btn btn-default" data-i18n="Refresh"></button>
    </div>
  </section>
  <section id="veil-settings" class="content-box veil-card" hidden>
    <h2 data-i18n="Configuration"></h2>
    <p class="text-muted" data-i18n="Import a JSON connection profile. Saving preserves current connections; apply changes separately."></p>
    <div class="veil-file">
      <button id="veil-choose" class="btn btn-default" data-i18n="Choose file"></button>
      <span id="veil-filename" data-i18n="No file selected"></span>
      <input id="veil-file" type="file" accept=".json,application/json" hidden>
    </div>
    <div id="veil-listener" class="veil-fields">
      <label><span data-i18n="Proxy protocol"></span><select id="veil-protocol" class="form-control"><option value="mixed">SOCKS5 + HTTP</option><option value="socks">SOCKS5</option><option value="http">HTTP</option></select></label>
      <label><span data-i18n="Listening address"></span><input id="veil-address" class="form-control" placeholder="127.0.0.1:1080"></label>
    </div>
    <label class="veil-boot"><input id="veil-enabled" type="checkbox"> <span data-i18n="Start proxy when the system boots"></span></label>
    <p id="veil-feedback" class="alert alert-success" role="status" hidden></p>
    <details><summary data-i18n="Advanced: edit configuration JSON"></summary><label for="veil-json" class="text-muted" data-i18n="Contains credentials. Share only with trusted administrators."></label><textarea id="veil-json" class="form-control" rows="14" spellcheck="false" autocomplete="off"></textarea></details>
    <div class="veil-actions">
      <button id="veil-save" class="btn btn-primary" data-i18n="Save configuration" disabled></button>
      <button id="veil-validate" class="btn btn-default" data-i18n="Validate" disabled></button>
      <button id="veil-reload" class="btn btn-default" data-i18n="Reload saved configuration"></button>
    </div>
  </section>
</div>
<script>window.veilUI = {catalog: {{ veilCatalog }}, writable: {{ veilWritable }}};</script>
<script src="/ui/js/veil/page.js"></script>
