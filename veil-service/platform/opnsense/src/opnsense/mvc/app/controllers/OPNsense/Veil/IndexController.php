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
        $catalog = in_array($language, ['zh', 'zh_cn', 'zh_hans'], true)
            ? json_decode(file_get_contents('/usr/local/opnsense/www/js/veil/zh.json'), true) : [];
        $this->view->veilCatalog = json_encode($catalog, JSON_HEX_TAG | JSON_HEX_AMP | JSON_HEX_APOS | JSON_HEX_QUOT);
        $this->view->veilWritable = (new ACL())->isPageAccessible($this->session->get('Username'), '/api/veil/settings/get') ? 'true' : 'false';
        $this->view->pick('OPNsense/Veil/index');
    }
}
