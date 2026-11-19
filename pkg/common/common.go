// Consumer helpers ported from cloud-event-proxy, Apache-2.0.
package common

import (
	log "github.com/sirupsen/logrus"
	"os"
	"strconv"
	"strings"
)

func SanitizeTransportHost(host, nodeIP, nodeName string) string {
	if nodeIP != "" {
		host = strings.ReplaceAll(host, "NODE_IP", nodeIP)
	}
	if nodeName != "" {
		host = strings.ReplaceAll(host, "NODE_NAME", strings.Split(nodeName, ".")[0])
	}
	return host
}
func GetBoolEnv(key string) bool { value, _ := strconv.ParseBool(os.Getenv(key)); return value }
func InitLogger() {
	level, err := log.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		level = log.DebugLevel
	}
	log.SetLevel(level)
}
