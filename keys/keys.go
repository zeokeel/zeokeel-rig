// Package keys exposes the kernel service keys that Rig's built-in contracts
// are provided under, so third-party modules can declare `Requires` without
// importing Rig's implementation packages.
//
// The kernel is the only dependency: a module that only needs these keys can
// import this package (and kernelx) and stay free of agent/tool/provider code.
package keys

import (
	"github.com/zeokeel/zeokeel-rig/kernelx"
	"github.com/zeokeel/zeokeel-rig/modules/appinfo"

	"github.com/zeokeel/zeokeel-rig/contracts/v1"
)

// AppInfoKey is the build identity service provided by modules/appinfo.
var AppInfoKey = appinfo.Key

// SettingsKey is the key/value settings service provided by the host.
var SettingsKey = kernelx.NewServiceKey[v1.Settings]("rig/settings")

// LoggerKey is the logger service provided by the host.
var LoggerKey = kernelx.NewServiceKey[v1.Logger]("rig/log")
