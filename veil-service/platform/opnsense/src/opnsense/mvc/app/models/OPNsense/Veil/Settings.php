<?php

namespace OPNsense\Veil;

class Settings extends \OPNsense\Base\BaseModel
{
    public function revision(): string
    {
        return hash('sha256', (string)$this->enabled . "\n" . (string)$this->profile);
    }

    public function snapshot(): array
    {
        return [
            'enabled' => (string)$this->enabled === '1',
            'profile' => (string)$this->profile,
            'revision' => $this->revision(),
        ];
    }
}
