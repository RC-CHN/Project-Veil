<?php

namespace OPNsense\Veil\Api;

use OPNsense\Veil\Bridge;

class ServiceController extends \OPNsense\Base\ApiControllerBase
{
    public function statusAction()
    {
        return $this->invoke('status');
    }

    public function startAction()
    {
        return $this->invoke('start');
    }

    public function stopAction()
    {
        return $this->invoke('stop');
    }

    public function restartAction()
    {
        return $this->invoke('restart');
    }

    private function invoke(string $action): array
    {
        if ($action !== 'status' && !$this->request->isPost()) {
            $this->response->setStatusCode(405);
            return ['error' => ['code' => 'method', 'message' => 'POST required']];
        }
        try {
            $payload = in_array($action, ['start', 'restart'], true)
                ? ['expected_revision' => $this->request->getPost('expected_revision')] : null;
            return Bridge::call($action, $payload);
        } catch (\Throwable $error) {
            return ['error' => ['code' => 'unavailable', 'message' => 'Veil management service is unavailable']];
        }
    }
}
