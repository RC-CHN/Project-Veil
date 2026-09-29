<?php

namespace OPNsense\Veil;

use OPNsense\Core\Config;

/** config.xml owns desired profiles; the engine owns validation and runtime. */
class Catalog
{
    private static function fail(string $code, string $message): array
    {
        return ['version' => 1, 'error' => ['code' => $code, 'message' => $message]];
    }

    public static function execute(string $action, array $request = []): array
    {
        // Status and probes never write config.xml or hold its global lock.
        if ($action === 'connections') { return Engine::call('connections'); }
        if ($action === 'connection' && ($request['action'] ?? '') === 'connection_test' && ($request['version'] ?? null) === 1) {
            return Engine::call('connection_test', ['id' => $request['id'] ?? '']);
        }
        if ($action === 'connection' && ($request['action'] ?? '') === 'connection_migrate' && ($request['version'] ?? null) === 1) { $action = 'configure'; }
        $system = Config::getInstance()->lock();
        try {
            $model = new Settings();
            $current = Engine::call('connections_get');
            if (isset($current['error'])) {
                return $current;
            }
            $desired = (string)$model->catalog;
            // Old client settings migrate once. Never stop a live legacy proxy
            // during a status read; configure is the explicit startup operation.
            if ($desired === '' && (string)$model->profile !== '') {
                $cfg = json_decode((string)$model->profile, true, 64, JSON_THROW_ON_ERROR);
                if (($cfg['role'] ?? '') === 'client' && empty($cfg['target'])) {
                    $legacy = Engine::call('status');
                    if (($legacy['status']['state'] ?? '') === 'running') {
                        if ($action !== 'configure') {
                            return self::fail('migration_required', 'Stop the standalone proxy before migrating its client configuration.');
                        }
                        $stopped = Engine::call('stop');
                        if (isset($stopped['error'])) { return $stopped; }
                    }
                    $valid = Engine::call('connections_validate', ['config' => ['version' => 1, 'profiles' => [[
                        'id' => 'default', 'name' => $cfg['server'], 'kind' => 'connection',
                        'enabled' => (string)$model->enabled === '1', 'config' => $cfg,
                        'inlets' => [['protocol' => ($cfg['inbound'] ?? '') ?: 'socks', 'listen' => ($cfg['listen'] ?? '') ?: '127.0.0.1:1080']],
                    ]]]]);
                    if (isset($valid['error'])) { return $valid; }
                    $model->catalog->setValue(json_encode($valid['config'], JSON_THROW_ON_ERROR));
                    $model->profile->setValue('');
                    $model->enabled->setValue('0');
                    $model->engineRevision->setValue('');
                    $model->serializeToConfig(); $system->save();
                    $desired = (string)$model->catalog;
                }
            }
            $store = $desired === '' ? ['version' => 1, 'profiles' => []]
                : json_decode($desired, true, 64, JSON_THROW_ON_ERROR);
            if ($store != $current['config']) {
                $synced = Engine::call('connections_save', [
                    'config' => $store, 'expected_revision' => $current['revision'], 'apply' => false,
                ]);
                if (isset($synced['error'])) { return $synced; }
                $current = $synced;
            }
            if ($action === 'configure') {
                $result = Engine::call('connections_save', [
                    'config' => $store, 'expected_revision' => $current['revision'], 'apply' => true,
                ]);
                unset($result['config']);
                return $result;
            }
            if ($action === 'connections') {
                return ['version' => 1, 'connections' => $current['connections'] ?? []];
            }
            $operation = $request['action'] ?? '';
            $allowed = ['connection_get', 'connection_export', 'connection_test', 'connection_save',
                'connection_start', 'connection_stop', 'connection_delete'];
            if (!in_array($operation, $allowed, true) || ($request['version'] ?? null) !== 1) {
                return self::fail('invalid_request', 'Unsupported connection request');
            }
            if (in_array($operation, ['connection_get', 'connection_export', 'connection_test'], true)) {
                return Engine::call($operation, ['id' => $request['id'] ?? '']);
            }
            $profile = $request['profile'] ?? null;
            $id = $operation === 'connection_save' ? ($profile['id'] ?? '') : ($request['id'] ?? '');
            $rows = array_column($current['connections'] ?? [], null, 'id');
            $revision = $rows[$id]['revision'] ?? '';
            if (!isset($request['expected_revision']) || $request['expected_revision'] !== $revision) {
                return self::fail('conflict', 'Configuration changed; reload before editing');
            }
            $profiles = array_column($store['profiles'], null, 'id');
            if ($operation !== 'connection_save' && !isset($profiles[$id])) {
                return self::fail('not_found', 'Connection not found');
            }
            $relay = $request['relay'] ?? null;
            $previousRelay = null;
            if ($operation === 'connection_save') {
                if (!is_array($profile)) { return self::fail('invalid_config', 'Profile required'); }
                if ($relay !== null) {
                    if (!is_array($relay) || ($relay['kind'] ?? '') !== 'relay' || ($profile['relay_id'] ?? '') !== ($relay['id'] ?? null) || $id === $relay['id']) {
                        return self::fail('invalid_config', 'Invalid embedded relay');
                    }
                    $previousRelay = $profiles[$relay['id']] ?? null;
                    $profiles[$relay['id']] = $relay;
                }
                $profiles[$id] = $profile;
            } elseif ($operation === 'connection_delete') {
                unset($profiles[$id]);
            } else {
                if ($profiles[$id]['kind'] !== 'connection') {
                    return self::fail('invalid_config', 'A relay starts with its connections');
                }
                $profiles[$id]['enabled'] = $operation === 'connection_start';
            }
            $candidate = ['version' => 1, 'profiles' => array_values($profiles)];
            $valid = Engine::call('connections_validate', ['config' => $candidate]);
            if (isset($valid['error'])) { return $valid; }
            if ($previousRelay !== null) {
                $normalized = array_column($valid['config']['profiles'], null, 'id');
                if ($normalized[$relay['id']] != $previousRelay) {
                    return self::fail('invalid_config', 'Embedded relay already exists; import with a new ID');
                }
            }
            // Persist desired state before applying: an apply failure leaves a
            // visible saved draft that can be retried after fixing the cause.
            $previousCatalog = (string)$model->catalog;
            $model->catalog->setValue(json_encode($valid['config'], JSON_THROW_ON_ERROR));
            $model->serializeToConfig(); $system->save();
            $result = Engine::call('connections_save', [
                'config' => $valid['config'], 'expected_revision' => $current['revision'],
                'id' => $id, 'apply' => $operation !== 'connection_save' || ($request['apply'] ?? false) === true,
            ]);
            // Validation can race with another process binding a port. Restore
            // the prior platform draft on a definite pre-commit rejection, so
            // the next edit does not get stuck synchronizing that invalid draft.
            if (in_array($result['error']['code'] ?? '', ['invalid_config', 'conflict', 'save_failed'], true)) {
                $model->catalog->setValue(isset($result['config'])
                    ? json_encode($result['config'], JSON_THROW_ON_ERROR) : $previousCatalog);
                $model->serializeToConfig(); $system->save();
            }
            unset($result['config']);
            $resultRows = array_column($result['connections'] ?? [], null, 'id');
            $result['revision'] = $resultRows[$id]['revision'] ?? '';
            return $result;
        } finally {
            $system->unlock();
        }
    }
}
