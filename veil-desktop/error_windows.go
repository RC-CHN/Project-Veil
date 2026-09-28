package main

import "golang.org/x/sys/windows"

func showStartupError(err error) {
	language := "en"
	if languages, e := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME); e == nil && len(languages) > 0 {
		language = languages[0]
	}
	message, e := windows.UTF16PtrFromString(startupMessage(err, language))
	if e != nil {
		return
	}
	caption, _ := windows.UTF16PtrFromString("Veil")
	windows.MessageBox(0, message, caption, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
}
