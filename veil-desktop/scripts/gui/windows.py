"""Native WinINet and window operations for isolated Windows GUI acceptance."""

import ctypes
import json
import sys
from ctypes import wintypes as W


class Value(ctypes.Union):
    _fields_ = [
        ("number", W.DWORD),
        ("text", ctypes.c_void_p),
        ("filetime", W.FILETIME),
    ]


class Option(ctypes.Structure):
    _fields_ = [("id", W.DWORD), ("value", Value)]


class Options(ctypes.Structure):
    _fields_ = [
        ("size", W.DWORD),
        ("connection", W.LPWSTR),
        ("count", W.DWORD),
        ("error", W.DWORD),
        ("options", ctypes.POINTER(Option)),
    ]


inet = ctypes.WinDLL("wininet", use_last_error=True)
inet.InternetQueryOptionW.argtypes = [
    ctypes.c_void_p,
    W.DWORD,
    ctypes.c_void_p,
    ctypes.POINTER(W.DWORD),
]
inet.InternetSetOptionW.argtypes = [ctypes.c_void_p, W.DWORD, ctypes.c_void_p, W.DWORD]
k = ctypes.WinDLL("kernel32", use_last_error=True)
k.GlobalFree.argtypes = [ctypes.c_void_p]


def snapshot():
    arr = (Option * 4)()
    arr[0].id = 10
    for i in range(1, 4):
        arr[i].id = i + 1
    options = Options(ctypes.sizeof(Options), None, 4, 0, arr)
    n = W.DWORD(options.size)
    if not inet.InternetQueryOptionW(None, 75, ctypes.byref(options), ctypes.byref(n)):
        raise ctypes.WinError(ctypes.get_last_error())
    result = {"flags": arr[0].value.number}
    for i, name in enumerate(["server", "bypass", "pac"], 1):
        p = arr[i].value.text
        result[name] = ctypes.wstring_at(p) if p else ""
        if p:
            k.GlobalFree(p)
    return result


def write(s):
    arr = (Option * 4)()
    arr[0].id = 1
    arr[0].value.number = s["flags"]
    keep = []
    for i, name in enumerate(["server", "bypass", "pac"], 1):
        buf = ctypes.create_unicode_buffer(s[name])
        keep.append(buf)
        arr[i].id = i + 1
        arr[i].value.text = ctypes.cast(buf, ctypes.c_void_p).value
    options = Options(ctypes.sizeof(Options), None, 4, 0, arr)
    if not inet.InternetSetOptionW(None, 75, ctypes.byref(options), options.size):
        raise ctypes.WinError(ctypes.get_last_error())
    for n in [39, 37]:
        if not inet.InternetSetOptionW(None, n, None, 0):
            raise ctypes.WinError(ctypes.get_last_error())


u = ctypes.WinDLL("user32", use_last_error=True)
u.FindWindowW.argtypes = [W.LPCWSTR, W.LPCWSTR]
u.FindWindowW.restype = W.HWND
u.IsWindowVisible.argtypes = [W.HWND]
u.GetWindowRect.argtypes = [W.HWND, ctypes.POINTER(W.RECT)]
u.PostMessageW.argtypes = [W.HWND, W.UINT, W.WPARAM, W.LPARAM]


def window():
    h = u.FindWindowW(None, "Veil")
    rect = W.RECT()
    if h:
        u.GetWindowRect(h, ctypes.byref(rect))
    return {
        "hwnd": h,
        "visible": bool(u.IsWindowVisible(h)) if h else False,
        "rect": [rect.left, rect.top, rect.right, rect.bottom],
    }


if __name__ == "__main__":
    command = sys.argv[1]
    if command == "proxy":
        print(json.dumps(snapshot()))
    elif command == "show":
        u.ShowWindow.argtypes = [W.HWND, ctypes.c_int]
        u.ShowWindow(u.FindWindowW(None, "Veil"), 5)
    elif command == "resize":
        u.MoveWindow.argtypes = [
            W.HWND,
            ctypes.c_int,
            ctypes.c_int,
            ctypes.c_int,
            ctypes.c_int,
            W.BOOL,
        ]
        u.MoveWindow(u.FindWindowW(None, "Veil"), 0, 0, 680, 720, True)
    elif command == "window":
        print(json.dumps(window()))
    elif command == "close":
        u.PostMessageW(u.FindWindowW(None, "Veil"), 0x10, 0, 0)
