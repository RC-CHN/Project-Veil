<?php
// Real Catalog + Engine + Go daemon. Only the OPNsense config.xml framework is
// replaced, allowing deterministic storage failures without a firewall VM.
namespace OPNsense\Core {
    class Config {
        public static array $saved = ['catalog' => '', 'profile' => '', 'enabled' => '0', 'engineRevision' => ''];
        public static array $pending = [];
        public static int $locks = 0;
        public static int $writes = 0;
        public static bool $failSave = false;
        public static $afterSave = null;
        public static function getInstance(): self { return new self(); }
        public function lock(): self { self::$locks++; return $this; }
        public function unlock(): void { self::$locks--; }
        public function save(): void {
            if (self::$failSave) { throw new \RuntimeException('injected config.xml storage failure'); }
            self::$saved = self::$pending; self::$writes++;
            $hook = self::$afterSave; self::$afterSave = null; if ($hook !== null) { $hook(); }
        }
    }
}
namespace OPNsense\Veil {
    class Field {
        public function __construct(private string $value) {}
        public function __toString(): string { return $this->value; }
        public function setValue(string $value): void { $this->value = $value; }
    }
    class Settings {
        public Field $catalog, $profile, $enabled, $engineRevision;
        public function __construct() { foreach (\OPNsense\Core\Config::$saved as $k => $v) { $this->$k = new Field($v); } }
        public function serializeToConfig(): void {
            foreach (get_object_vars($this) as $k => $v) { \OPNsense\Core\Config::$pending[$k] = (string)$v; }
        }
    }
}
namespace {
    use OPNsense\Core\Config;
    use OPNsense\Veil\Catalog;
    use OPNsense\Veil\Engine;
    require $argv[1] . '/Engine.php';
    require $argv[1] . '/Catalog.php';
    function check(bool $ok, string $message): void { if (!$ok) { throw new RuntimeException($message); } }
    function request(string $action, array $data = []): array {
        return Catalog::execute('connection', ['version' => 1, 'action' => $action] + $data);
    }
    function row(string $id): array {
        foreach (Catalog::execute('connections')['connections'] ?? [] as $r) { if ($r['id'] === $id) { return $r; } }
        throw new RuntimeException('missing row');
    }
    $config = ['role' => 'client', 'server' => '127.0.0.1:1', 'secret' => rtrim(strtr(base64_encode(str_repeat(chr(17), 32)), '+/', '-_'), '='),
        'tls' => ['mode' => 'tls', 'server_name' => 'localhost']];
    $profile = ['id' => 'one', 'name' => 'One', 'kind' => 'connection', 'enabled' => true,
        'config' => $config, 'inlets' => [['protocol' => 'mixed', 'listen' => '127.0.0.1:39181']]];
    $saved = request('connection_save', ['profile' => $profile, 'expected_revision' => '', 'apply' => true]);
    check(!isset($saved['error']) && row('one')['state'] === 'running', 'create/start: ' . json_encode($saved));
    $revision = row('one')['revision']; $before = Config::$writes;
    $status = Catalog::execute('connections');
    check(Config::$writes === $before && !str_contains(json_encode($status), $config['secret']), 'status mutated XML or leaked credentials');
    $profile['name'] = 'Draft';
    $draft = request('connection_save', ['profile' => $profile, 'expected_revision' => $revision]);
    check(!isset($draft['error']) && row('one')['state'] === 'running', 'save-only stopped runtime');
    $stale = request('connection_stop', ['id' => 'one', 'expected_revision' => $revision]);
    check(($stale['error']['code'] ?? '') === 'conflict' && row('one')['state'] === 'running', 'stale revision changed runtime');
    $before = Engine::call('connections_get');
    Config::$failSave = true;
    try { request('connection_stop', ['id' => 'one', 'expected_revision' => row('one')['revision']]); throw new RuntimeException('storage failure ignored'); }
    catch (RuntimeException $e) { check(str_contains($e->getMessage(), 'injected'), 'unexpected storage exception'); }
    Config::$failSave = false;
    check(Engine::call('connections_get')['revision'] === $before['revision'] && Config::$locks === 0, 'failed XML write mutated engine or leaked lock');
    $two = $profile; $two['id'] = 'two'; $two['name'] = 'Two'; $two['inlets'][0]['listen'] = '127.0.0.1:39182';
    $raced = null;
    Config::$afterSave = function () use (&$raced): void { $raced = stream_socket_server('tcp://127.0.0.1:39182', $errno, $error); };
    $racing = request('connection_save', ['profile' => $two, 'expected_revision' => '', 'apply' => true]);
    check(($racing['error']['code'] ?? '') === 'invalid_config' && count(json_decode(Config::$saved['catalog'], true)['profiles']) === 1, 'post-XML bind race trapped an invalid platform draft');
    fclose($raced);
    $blocked = stream_socket_server('tcp://127.0.0.1:39182', $errno, $error);
    check($blocked !== false, 'test listener unavailable');
    $failed = request('connection_save', ['profile' => $two, 'expected_revision' => '', 'apply' => true]);
    check(($failed['error']['code'] ?? '') === 'invalid_config', 'bind failure not reported');
    check(count(json_decode(Config::$saved['catalog'], true)['profiles']) === 1, 'invalid port persisted');
    fclose($blocked);
    $retry = request('connection_save', ['profile' => $two, 'expected_revision' => '', 'apply' => true]);
    check(!isset($retry['error']) && row('two')['state'] === 'running', 'retry after bind conflict failed');
    $stopped = request('connection_stop', ['id' => 'one', 'expected_revision' => row('one')['revision']]);
    check(!isset($stopped['error']) && row('one')['state'] === 'stopped' && row('two')['state'] === 'running', 'stop affected another connection');
    $export = request('connection_export', ['id' => 'two']);
    check(($export['profile']['id'] ?? '') === 'two', 'export failed');
    foreach (['one', 'two'] as $id) {
        $deleted = request('connection_delete', ['id' => $id, 'expected_revision' => row($id)['revision']]);
        check(!isset($deleted['error']), 'delete failed');
    }
    Config::$saved['catalog'] = ''; Config::$saved['profile'] = json_encode($config + ['inbound' => 'mixed', 'listen' => '127.0.0.1:39183']);
    Config::$saved['enabled'] = '1';
    $migrated = Catalog::execute('configure');
    check(!isset($migrated['error']) && row('default')['state'] === 'running' && Config::$saved['profile'] === '', 'legacy migration failed: ' . json_encode($migrated));
    check(Config::$locks === 0, 'config lock leaked');
    echo "OPNsense catalog: real daemon lifecycle, XML-first failure, CAS, retries, export and migration passed\n";
}
