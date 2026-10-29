package config

import (
	"encoding/base64"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const sealPrefix = "dpapi:"

// seal encrypts a secret for the current Windows user with DPAPI.
func seal(v string) string {
	if v == "" || strings.HasPrefix(v, sealPrefix) {
		return v
	}
	in := []byte(v)
	blob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&blob, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return v
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return sealPrefix + base64.StdEncoding.EncodeToString(unsafe.Slice(out.Data, out.Size))
}

func unseal(v string) string {
	if !strings.HasPrefix(v, sealPrefix) {
		return v
	}
	raw, err := base64.StdEncoding.DecodeString(v[len(sealPrefix):])
	if err != nil || len(raw) == 0 {
		return ""
	}
	blob := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&blob, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size))
}
