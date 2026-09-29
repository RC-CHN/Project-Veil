<?php

namespace OPNsense\Veil;

use OPNsense\Core\ACL;

class IndexController extends \OPNsense\Base\IndexController
{
    protected function templateCSSIncludes()
    {
        return array_merge(parent::templateCSSIncludes(), ['/ui/css/veil.css']);
    }

    public function indexAction()
    {
        $language = strtolower(str_replace('-', '_', $this->langcode));
        $this->view->veilLanguage = json_encode(str_starts_with($language, 'zh') ? 'zh' : 'en');
        $this->view->veilWritable = (new ACL())->isPageAccessible($this->session->get('Username'), '/api/veil/settings/get') ? 'true' : 'false';
        $this->view->pick('OPNsense/Veil/index');
    }
}
