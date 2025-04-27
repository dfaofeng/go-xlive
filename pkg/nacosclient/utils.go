package nacosclient

import (
	"fmt"
	"net"
)

// getOutboundIP 尝试获取本机首选的出站 IP 地址。
// 注意：此方法在所有网络配置下（例如多网卡、Docker）可能不总是可靠。
func getOutboundIP() (string, error) {
	// 连接到一个已知的外部地址（实际上不发送数据）
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", fmt.Errorf("无法通过 UDP dial 获取出站 IP: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr == nil || localAddr.IP == nil {
		return "", fmt.Errorf("无法确定本地 IP 地址")
	}
	return localAddr.IP.String(), nil
}
