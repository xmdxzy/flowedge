# -*- coding: utf-8 -*-
"""Create a real Windows .lnk via the shell's own COM object.

Hand-rolled Shell Link binaries are rejected by the shell on this host
(ACP = UTF-8, no LinkTargetIDList), so we drive IShellLinkW through ctypes
instead.  No third-party dependency required.

usage: make-lnk.py <lnk> <target> <workdir> [icon] [desc] [args]
"""
import ctypes
import sys
from ctypes import POINTER, byref, c_int, c_ubyte, c_void_p, c_wchar_p
from ctypes import wintypes

ole32 = ctypes.WinDLL('ole32')
shell32 = ctypes.WinDLL('shell32')
HRESULT = ctypes.c_long


class GUID(ctypes.Structure):
    _fields_ = [('Data1', wintypes.DWORD), ('Data2', wintypes.WORD),
                ('Data3', wintypes.WORD), ('Data4', c_ubyte * 8)]


def guid(text):
    g = GUID()
    hr = ole32.CLSIDFromString(c_wchar_p(text), byref(g))
    if hr < 0:
        raise OSError('CLSIDFromString failed for %s' % text)
    return g


CLSID_ShellLink = guid('{00021401-0000-0000-C000-000000000046}')
IID_IShellLinkW = guid('{000214F9-0000-0000-C000-000000000046}')
IID_IPersistFile = guid('{0000010B-0000-0000-C000-000000000046}')

CLSCTX_INPROC_SERVER = 1


def call(obj, index, argtypes, restype=HRESULT, *args):
    """Invoke vtable slot `index` on a raw COM pointer."""
    vtable = ctypes.cast(obj, POINTER(POINTER(c_void_p))).contents
    proto = ctypes.WINFUNCTYPE(restype, c_void_p, *argtypes)
    return proto(vtable[index])(obj, *args)


def _win(p):
    """Git-Bash hands us forward slashes; the shell wants backslashes."""
    return p.replace('/', '\\') if p else p


def create(lnk, target, workdir, icon=None, desc=None, args=None, show=1):
    lnk, target, workdir = _win(lnk), _win(target), _win(workdir)
    icon = _win(icon)
    hr = ole32.CoInitialize(None)
    try:
        p = c_void_p()
        hr = ole32.CoCreateInstance(byref(CLSID_ShellLink), None,
                                    CLSCTX_INPROC_SERVER,
                                    byref(IID_IShellLinkW), byref(p))
        if hr < 0:
            raise OSError('CoCreateInstance failed: 0x%08X' % (hr & 0xFFFFFFFF))

        call(p, 20, [c_wchar_p], HRESULT, c_wchar_p(target))          # SetPath
        if workdir:
            call(p, 9, [c_wchar_p], HRESULT, c_wchar_p(workdir))      # SetWorkingDirectory
        if args:
            call(p, 11, [c_wchar_p], HRESULT, c_wchar_p(args))        # SetArguments
        if desc:
            call(p, 7, [c_wchar_p], HRESULT, c_wchar_p(desc))         # SetDescription
        if icon:
            call(p, 17, [c_wchar_p, c_int], HRESULT, c_wchar_p(icon), 0)  # SetIconLocation
        call(p, 15, [c_int], HRESULT, show)                           # SetShowCmd

        pf = c_void_p()
        call(p, 0, [POINTER(GUID), POINTER(c_void_p)], HRESULT,
             byref(IID_IPersistFile), byref(pf))                      # QueryInterface
        if not pf.value:
            raise OSError('QueryInterface(IPersistFile) failed')
        hr = call(pf, 6, [c_wchar_p, wintypes.BOOL], HRESULT,
                  c_wchar_p(lnk), wintypes.BOOL(1))                   # Save
        if hr < 0:
            raise OSError('IPersistFile::Save failed: 0x%08X' % (hr & 0xFFFFFFFF))

        call(pf, 2, [], ctypes.c_ulong)                               # Release
        call(p, 2, [], ctypes.c_ulong)                                # Release
    finally:
        ole32.CoUninitialize()


def main(argv):
    if len(argv) < 3:
        print('usage: make-lnk.py <lnk> <target> <workdir> [icon] [desc] [args]')
        return 1
    lnk, target, workdir = argv[0], argv[1], argv[2]
    icon = argv[3] if len(argv) > 3 and argv[3] else None
    desc = argv[4] if len(argv) > 4 and argv[4] else None
    args = argv[5] if len(argv) > 5 and argv[5] else None
    create(lnk, target, workdir, icon, desc, args)
    print('CREATED %s -> %s' % (lnk, target))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
