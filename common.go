package main

const (
	appName    = "filerewrite"
	bytesPerMB = 1024 * 1024
)

// appVersion is set at build time via ldflags:
//
//	go build -ldflags "-X main.appVersion=v1.2.3"
var appVersion = "dev"
