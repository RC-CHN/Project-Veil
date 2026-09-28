package main

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
#include <stdlib.h>

static void veil_startup_error(const char *message) {
    if (!gtk_init_check(NULL, NULL)) return;
    GtkWidget *dialog = gtk_message_dialog_new(NULL, GTK_DIALOG_MODAL,
        GTK_MESSAGE_ERROR, GTK_BUTTONS_CLOSE, "%s", message);
    gtk_window_set_title(GTK_WINDOW(dialog), "Veil");
    gtk_dialog_run(GTK_DIALOG(dialog));
    gtk_widget_destroy(dialog);
}
*/
import "C"

import (
	"os"
	"runtime"
	"unsafe"
)

func showStartupError(err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	language := "en"
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := os.Getenv(key); value != "" {
			language = value
			break
		}
	}
	message := C.CString(startupMessage(err, language))
	defer C.free(unsafe.Pointer(message))
	C.veil_startup_error(message)
}
