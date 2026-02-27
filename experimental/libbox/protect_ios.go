//go:build ios

package libbox

import "github.com/sagernet/tailscale/net/netns"

func setupPlatformProtect(platformInterface PlatformInterface) {
	if platformInterface != nil {
		netns.SetIOSProtectFunc(func(fd int) error {
			return platformInterface.AutoDetectInterfaceControl(int32(fd))
		})
	} else {
		netns.SetIOSProtectFunc(nil)
	}
}
