<?php

namespace OPNsense\Veil;

use OPNsense\Core\Backend;

/** Only opaque request IDs cross configd's command/logging boundary. */
class Bridge
{
    public const REQUESTS = '/var/run/veil/requests';
    public const MAX_REQUEST = 73728;

    public static function call(string $action, ?array $payload = null): array
    {
        if (!in_array($action, ['status', 'validate', 'start', 'stop', 'restart'], true)) {
            throw new \InvalidArgumentException('Unsupported control action');
        }
        $path = null;
        $arguments = [];
        try {
            if ($payload !== null) {
                $body = json_encode($payload, JSON_THROW_ON_ERROR);
                if (strlen($body) > self::MAX_REQUEST) {
                    throw new \InvalidArgumentException('Request too large');
                }
                $id = bin2hex(random_bytes(16));
                $path = self::REQUESTS . '/' . $id;
                $mask = umask(0077);
                try {
                    $file = fopen($path, 'x');
                } finally {
                    umask($mask);
                }
                if ($file === false) {
                    throw new \RuntimeException('Veil management service is unavailable');
                }
                try {
                    if (fwrite($file, $body) !== strlen($body)) {
                        throw new \RuntimeException('Unable to stage control request');
                    }
                } finally {
                    fclose($file);
                }
                $arguments[] = $id;
            }
            $reply = (new Backend())->configdpRun('veil ' . $action, $arguments, false, 25);
            $result = json_decode($reply, true, 64, JSON_THROW_ON_ERROR);
            if (!is_array($result)) {
                throw new \RuntimeException('Invalid control response');
            }
            return $result;
        } finally {
            if ($path !== null && is_file($path)) {
                unlink($path);
            }
        }
    }
}
