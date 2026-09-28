package systemproxy

/*
#cgo pkg-config: gio-2.0
#include <gio/gio.h>
#include <stdlib.h>

static GSettings *veil_proxy_settings(void) {
    GSettingsSchemaSource *source = g_settings_schema_source_get_default();
    if (!source) return NULL;
    GSettingsSchema *schema = g_settings_schema_source_lookup(source, "org.gnome.system.proxy", TRUE);
    if (!schema) return NULL;
    GSettings *settings = g_settings_new_full(schema, NULL, NULL);
    g_settings_schema_unref(schema);
    return settings;
}
static GSettings *veil_proxy_child(GSettings *root, const char *path, const char **key) {
    const char *slash = strchr(path, '/');
    if (!slash) { *key = path; return g_object_ref(root); }
    char *child = g_strndup(path, slash - path);
    GSettings *settings = g_settings_get_child(root, child);
    g_free(child);
    *key = slash + 1;
    return settings;
}
static char *veil_proxy_read(GSettings *root, const char *path) {
    const char *key;
    GSettings *settings = veil_proxy_child(root, path, &key);
    GVariant *value = g_settings_get_value(settings, key);
    char *text = g_variant_print(value, TRUE);
    g_variant_unref(value);
    g_object_unref(settings);
    return text;
}
static int veil_proxy_stage(GSettings *root, const char *path, const char *text) {
    const char *key;
    GSettings *settings = veil_proxy_child(root, path, &key);
    GVariant *value = g_variant_parse(NULL, text, NULL, NULL, NULL);
    gboolean ok = value && g_settings_is_writable(settings, key);
    if (ok) ok = g_settings_set_value(settings, key, value);
    if (value) g_variant_unref(value);
    g_object_unref(settings);
    return ok;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"unsafe"
)

type gnome struct{}

func Native() Backend {
	desktop := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP"))
	if !strings.Contains(desktop, "gnome") && !strings.Contains(desktop, "unity") {
		return nil
	}
	settings := C.veil_proxy_settings()
	if settings == nil {
		return nil
	}
	C.g_object_unref(C.gpointer(settings))
	return gnome{}
}
func (gnome) Name() string { return "gnome" }

// Credentials, PAC URLs and bypass lists are not changed or journaled.
var keys = []string{"mode", "http/host", "http/port", "http/use-authentication", "https/host", "https/port", "socks/host", "socks/port"}

func (gnome) Read() (Snapshot, error) {
	settings := C.veil_proxy_settings()
	if settings == nil {
		return nil, ErrUnsupported
	}
	defer C.g_object_unref(C.gpointer(settings))
	result := Snapshot{}
	for _, key := range keys {
		path := C.CString(key)
		value := C.veil_proxy_read(settings, path)
		result[key] = C.GoString(value)
		C.free(unsafe.Pointer(path))
		C.g_free(C.gpointer(value))
	}
	return result, nil
}
func (g gnome) Write(s Snapshot) error {
	settings := C.veil_proxy_settings()
	if settings == nil {
		return ErrUnsupported
	}
	defer C.g_object_unref(C.gpointer(settings))
	// Children inherit the parent's delayed backend. Apply all keys as one
	// GSettings change set instead of issuing partial per-key shell commands.
	C.g_settings_delay(settings)
	for _, key := range keys {
		value, ok := s[key]
		if !ok {
			return errors.New("incomplete GNOME proxy snapshot")
		}
		path, text := C.CString(key), C.CString(value)
		accepted := C.veil_proxy_stage(settings, path, text)
		C.free(unsafe.Pointer(path))
		C.free(unsafe.Pointer(text))
		if accepted == 0 {
			return fmt.Errorf("system proxy setting %s is invalid or locked by policy", key)
		}
	}
	C.g_settings_apply(settings)
	C.g_settings_sync()
	actual, err := g.Read()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, s) {
		return errors.New("system proxy changes were not accepted")
	}
	return nil
}
func (gnome) Manual(s Snapshot, endpoint string) Snapshot {
	host, port, _ := net.SplitHostPort(endpoint)
	s["mode"] = "'manual'"
	for _, protocol := range []string{"http", "https"} {
		s[protocol+"/host"] = "'" + host + "'"
		s[protocol+"/port"] = port
	}
	s["http/use-authentication"] = "false"
	s["socks/host"] = "''"
	s["socks/port"] = "0"
	return s
}
func (gnome) Direct(s Snapshot) Snapshot { s["mode"] = "'none'"; return s }
