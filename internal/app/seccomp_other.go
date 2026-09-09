//go:build linux && !amd64 && !arm64

package app

var nativeSeccompArchitecture = seccompArchitecture{}
