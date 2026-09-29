#!/usr/local/bin/php
<?php

require_once('script/load_phalcon.php');

use OPNsense\Core\Config;
use OPNsense\Veil\Bridge;
use OPNsense\Veil\Engine;
use OPNsense\Veil\Settings;
use OPNsense\Veil\Catalog;

function requestPayload(string $id): array
{
    if (!preg_match('/^[0-9a-f]{32}$/D', $id)) {
        throw new InvalidArgumentException('Invalid request ID');
    }
    $path = Bridge::REQUESTS . '/' . $id;
    $stat = lstat($path);
    if ($stat === false || ($stat['mode'] & 0170000) !== 0100000 || ($stat['mode'] & 0077) !== 0
        || $stat['uid'] !== 0 || $stat['size'] > Bridge::MAX_REQUEST) {
        throw new InvalidArgumentException('Invalid staged request');
    }
    try {
        $body = file_get_contents($path);
        $request = json_decode($body, false, 64, JSON_THROW_ON_ERROR);
        if (!is_object($request)) {
            throw new InvalidArgumentException('Expected a request object');
        }
        return (array)$request;
    } finally {
        unlink($path);
    }
}

function ensureDaemon(): void
{
    if (!isset(Engine::call('status')['error'])) {
        return;
    }
    $process = proc_open(['/usr/local/etc/rc.d/veil', 'onestart'], [
        0 => ['file', '/dev/null', 'r'],
        1 => ['file', '/dev/null', 'w'],
        2 => ['file', '/dev/null', 'w'],
    ], $pipes);
    if (!is_resource($process)) {
        throw new RuntimeException('Unable to start management service');
    }
    proc_close($process);
    for ($attempt = 0; $attempt < 20; $attempt++) {
        if (!isset(Engine::call('status')['error'])) {
            return;
        }
        usleep(100000);
    }
    throw new RuntimeException('Management service did not become ready');
}

function executeAction(string $action, array $payload): array
{
    if ($action === 'connections' || $action === 'connection') {
        if ($action === 'connection') { ensureDaemon(); }
        return Catalog::execute($action, json_decode(json_encode($payload['request'] ?? [], JSON_THROW_ON_ERROR), true, 64, JSON_THROW_ON_ERROR));
    }
    if ($action === 'status') {
        $result = Engine::call('status');
        $model = new Settings();
        $result['revision'] = $model->revision();
        $result['enabled'] = (string)$model->enabled === '1';
        $result['configured'] = (string)$model->profile !== '';
        $result['pending'] = $result['configured'] && (string)$model->engineRevision !== ($result['status']['active_revision'] ?? '');
        return $result;
    }
    if ($action === 'stop') {
        return Engine::call('stop');
    }
    ensureDaemon();
    if ($action === 'validate') {
        return Engine::call('validate', ['config' => $payload['config'] ?? null]);
    }
    if ($action === 'configure') {
        $catalog = Catalog::execute('configure');
        if (isset($catalog['error']) || (string)(new Settings())->profile === '') { return $catalog; }
    }
    $system = Config::getInstance()->lock();
    try {
        $model = new Settings();
        if ($action === 'configure' && (string)$model->enabled !== '1') {
            return Engine::call('stop');
        }
        if ($action !== 'configure' && ($payload['expected_revision'] ?? null) !== $model->revision()) {
            return ['error' => ['code' => 'conflict', 'message' => 'Saved configuration changed; reload it']];
        }
        if ((string)$model->profile === '') {
            return ['error' => ['code' => 'no_config', 'message' => 'Import and save a configuration first']];
        }
        $config = json_decode((string)$model->profile, false, 64, JSON_THROW_ON_ERROR);
        $saved = Engine::call('save', ['config' => $config]);
        if (isset($saved['error'])) {
            return $saved;
        }
        return Engine::call($action === 'restart' ? 'restart' : 'start', ['expected_revision' => $saved['revision']]);
    } finally {
        $system->unlock();
    }
}

try {
    $action = $argv[1] ?? '';
    if (!in_array($action, ['status', 'validate', 'start', 'stop', 'restart', 'configure', 'connections', 'connection'], true)) {
        throw new InvalidArgumentException('Unsupported action');
    }
    $needsPayload = in_array($action, ['validate', 'start', 'restart', 'connection'], true);
    if (count($argv) !== ($needsPayload ? 3 : 2)) {
        throw new InvalidArgumentException('Invalid action arguments');
    }
    $payload = $needsPayload ? requestPayload($argv[2]) : [];
    $readOnly = in_array($action, ['status', 'connections'], true) || ($action === 'connection' && ($payload['request']->action ?? '') === 'connection_test');
    $lock = $readOnly ? null : fopen('/var/run/veil/platform.lock', 'c');
    if (!$readOnly && ($lock === false || !flock($lock, LOCK_EX))) {
        throw new RuntimeException('Unable to lock management operation');
    }
    try {
        $result = executeAction($action, $payload);
    } finally {
        if ($lock !== null) { flock($lock, LOCK_UN); fclose($lock); }
    }
} catch (Throwable $error) {
    $result = ['error' => ['code' => 'control_failed', 'message' => $error->getMessage()]];
}
echo json_encode($result, JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR) . "\n";
