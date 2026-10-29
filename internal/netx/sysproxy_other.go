//go:build !windows

package netx

import "net/url"

func systemProxy(*url.URL) (*url.URL, error) { return nil, nil }
