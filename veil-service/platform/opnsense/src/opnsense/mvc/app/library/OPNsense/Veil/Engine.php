<?php

namespace OPNsense\Veil;

/** Veil control v1 over its private Unix socket; proxy bytes never enter PHP. */
class Engine
{
    public static function call(string $action, array $parameters = []): array
    {
        $socket = @stream_socket_client('unix:///var/run/veil/control.sock', $errno, $message, 2);
        if ($socket === false) {
            return ['error' => ['code' => 'unavailable', 'message' => 'Veil control service is unavailable']];
        }
        try {
            stream_set_timeout($socket, 16);
            $request = json_encode(['version' => 1, 'action' => $action] + $parameters, JSON_THROW_ON_ERROR) . "\n";
            while ($request !== '') {
                $written = fwrite($socket, $request);
                if ($written === false || $written === 0) {
                    throw new \RuntimeException('Control request could not be sent');
                }
                $request = substr($request, $written);
            }
            $reply = fgets($socket, 4 * 1024 * 1024 + 1);
            if ($reply === false || !str_ends_with($reply, "\n")) {
                throw new \RuntimeException('Control response is incomplete');
            }
            $response = json_decode($reply, true, 64, JSON_THROW_ON_ERROR);
            if (!is_array($response) || ($response['version'] ?? null) !== 1) {
                throw new \RuntimeException('Invalid control response');
            }
            return $response;
        } finally {
            fclose($socket);
        }
    }
}
