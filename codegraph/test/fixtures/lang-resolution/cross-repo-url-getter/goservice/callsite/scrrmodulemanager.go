// Mirrors the goserving scrrmodulemanager.go call site: pulls the
// OriginConfig via the trivial getter, then constructs a
// cmorigin.Request whose Path field reads the YAML-bound Renderer.
// The URL resolver should recover `/scrr.php` for this call.
package callsite

import (
	"goservice/cmorigin"
	"goservice/config"
)

func GetFinalJS() {
	originConfig := config.GetOriginConfig()
	cmorigin.NewScrr(&cmorigin.Request{
		Protocol: originConfig.Protocol,
		Host:     originConfig.Host,
		Port:     originConfig.Port,
		Path:     originConfig.Renderer,
	})
}
