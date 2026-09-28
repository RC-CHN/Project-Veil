package main

import (
	"errors"
	"strings"
	"veil-service/local"
)

func startupMessage(err error, language string) string {
	zh := strings.HasPrefix(strings.ToLower(language), "zh")
	if errors.Is(err, local.ErrStateLocked) {
		if zh {
			return "Veil 已在运行。\n\n请使用已打开的窗口，或先退出占用此配置的程序。"
		}
		return "Veil is already running.\n\nUse its existing window, or quit the application using this profile."
	}
	if zh {
		return "无法启动 Veil。\n\n" + err.Error()
	}
	return "Cannot start Veil.\n\n" + err.Error()
}
