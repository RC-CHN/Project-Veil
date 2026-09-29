package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedAssets(t *testing.T) {
	for _, name := range []string{"connections.js", "connections.css", "i18n.js"} {
		r := httptest.NewRecorder()
		Handler().ServeHTTP(r, httptest.NewRequest("GET", "/shared/"+name, nil))
		if r.Code != 200 || r.Body.Len() == 0 {
			t.Fatal(name, r.Code)
		}
	}
	r := httptest.NewRecorder()
	Handler().ServeHTTP(r, httptest.NewRequest("GET", "/shared/assets.go", nil))
	if r.Code != 404 || strings.Contains(r.Body.String(), "package webui") {
		t.Fatal("non-asset file exposed")
	}
}
