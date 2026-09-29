<?php

namespace OPNsense\Veil\Api;

use OPNsense\Core\Config;
use OPNsense\Veil\Bridge;
use OPNsense\Veil\Settings;

class SettingsController extends \OPNsense\Base\ApiControllerBase
{
    public function connectionAction()
    {
        if (!$this->request->isPost()) { $this->response->setStatusCode(405); return ['error' => ['code' => 'method', 'message' => 'POST required']]; }
        try {
            $raw = $this->request->getPost('request');
            if (!is_string($raw) || strlen($raw) > 65536) { throw new \InvalidArgumentException('Invalid connection request'); }
            $request = json_decode($raw, true, 64, JSON_THROW_ON_ERROR);
            if (!is_array($request)) { throw new \InvalidArgumentException('Expected an object'); }
            return Bridge::call('connection', ['request' => $request]);
        } catch (\Throwable $error) { return ['error' => ['code' => 'invalid_request', 'message' => $error->getMessage()]]; }
    }

    public function getAction()
    {
        return (new Settings())->snapshot();
    }

    public function validateAction()
    {
        return $this->candidate(false);
    }

    public function saveAction()
    {
        return $this->candidate(true);
    }

    private function candidate(bool $save): array
    {
        if (!$this->request->isPost()) {
            $this->response->setStatusCode(405);
            return ['error' => ['code' => 'method', 'message' => 'POST required']];
        }
        try {
            $profile = $this->request->getPost('profile');
            $enabled = $this->request->getPost('enabled');
            if (!is_string($profile) || strlen($profile) > 65536 || !in_array($enabled, ['0', '1'], true)) {
                throw new \InvalidArgumentException('Invalid profile or startup setting');
            }
            $config = json_decode($profile, false, 64, JSON_THROW_ON_ERROR);
            if (!is_object($config)) {
                throw new \InvalidArgumentException('Configuration must be a JSON object');
            }
            $valid = Bridge::call('validate', ['config' => $config]);
            if (isset($valid['error']) || !$save) {
                return $valid;
            }
            $system = Config::getInstance()->lock();
            try {
                $model = new Settings();
                if ($this->request->getPost('expected_revision') !== $model->revision()) {
                    return ['error' => ['code' => 'conflict', 'message' => 'Saved configuration changed; reload it']];
                }
                $model->profile->setValue(json_encode($config, JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR));
                $model->enabled->setValue($enabled);
                $model->engineRevision->setValue($valid['revision']);
                $errors = $model->performValidation();
                if (count($errors) !== 0) {
                    throw new \InvalidArgumentException('Invalid settings');
                }
                $model->serializeToConfig();
                $system->save();
                return ['saved' => true, 'revision' => $model->revision()];
            } finally {
                $system->unlock();
            }
        } catch (\Throwable $error) {
            return ['error' => ['code' => 'invalid_config', 'message' => $error->getMessage()]];
        }
    }
}
